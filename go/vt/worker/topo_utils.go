// Copyright 2013, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package worker

import (
	"flag"
	"fmt"
	"math/rand"
	"time"

	"github.com/youtube/vitess/go/vt/servenv"
	"github.com/youtube/vitess/go/vt/topo"
	"github.com/youtube/vitess/go/vt/wrangler"
	"golang.org/x/net/context"
)

var (
	minHealthyEndPoints = flag.Int("min_healthy_rdonly_endpoints", 2, "minimum number of healthy rdonly endpoints required for checker")
	// The intent of this timeout is to wait for the healthcheck to automatically return rdonly instances which have been taken out by previous *Clone or *Diff runs.
	// Therefore, the default for this variable must be higher than -health_check_interval.
	waitForHealthyEndPointsTimeout = flag.Duration("wait_for_healthy_rdonly_endpoints_timeout", 60*time.Second, "maximum time to wait if less than --min_healthy_rdonly_endpoints are available")
)

// FindHealthyRdonlyEndPoint returns a random healthy endpoint.
// Since we don't want to use them all, we require at least
// minHealthyEndPoints servers to be healthy.
// May block up to -wait_for_healthy_rdonly_endpoints_timeout.
func FindHealthyRdonlyEndPoint(ctx context.Context, wr *wrangler.Wrangler, cell, keyspace, shard string) (topo.TabletAlias, error) {
	waitDeadline := time.Now().Add(*waitForHealthyEndPointsTimeout)
	var healthyEndpoints []topo.EndPoint
	for {
		endPoints, _, err := wr.TopoServer().GetEndPoints(ctx, cell, keyspace, shard, topo.TYPE_RDONLY)
		if err != nil {
			if err == topo.ErrNoNode {
				// If the node doesn't exist, count that as 0 available rdonly instances.
				endPoints = &topo.EndPoints{}
			} else {
				return topo.TabletAlias{}, fmt.Errorf("GetEndPoints(%v,%v,%v,rdonly) failed: %v", cell, keyspace, shard, err)
			}
		}
		healthyEndpoints = make([]topo.EndPoint, 0, len(endPoints.Entries))
		for _, entry := range endPoints.Entries {
			if len(entry.Health) == 0 {
				healthyEndpoints = append(healthyEndpoints, entry)
			}
		}
		if len(healthyEndpoints) < *minHealthyEndPoints {
			if time.Now().After(waitDeadline) {
				return topo.TabletAlias{}, fmt.Errorf("Not enough endpoints to chose from in (%v,%v/%v), have %v healthy ones, need at least %v", cell, keyspace, shard, len(healthyEndpoints), *minHealthyEndPoints)
			}
			wr.Logger().Infof("Waiting for enough endpoints to become available. available: %v required: %v Waiting up to %.1f more seconds.", len(healthyEndpoints), *minHealthyEndPoints, waitDeadline.Sub(time.Now()).Seconds())
			// Block for 1 second because 2 seconds is the -health_check_interval in integration tests.
			time.Sleep(1 * time.Second)
			continue
		}
		break
	}

	// random server in the list is what we want
	index := rand.Intn(len(healthyEndpoints))
	return topo.TabletAlias{
		Cell: cell,
		Uid:  healthyEndpoints[index].Uid,
	}, nil
}

// FindWorkerTablet will:
// - find a rdonly instance in the keyspace / shard
// - mark it as worker
// - tag it with our worker process
func FindWorkerTablet(ctx context.Context, wr *wrangler.Wrangler, cleaner *wrangler.Cleaner, cell, keyspace, shard string) (topo.TabletAlias, error) {
	tabletAlias, err := FindHealthyRdonlyEndPoint(ctx, wr, cell, keyspace, shard)
	if err != nil {
		return topo.TabletAlias{}, err
	}

	// We add the tag before calling ChangeSlaveType, so the destination
	// vttablet reloads the worker URL when it reloads the tablet.
	ourURL := servenv.ListeningURL.String()
	wr.Logger().Infof("Adding tag[worker]=%v to tablet %v", ourURL, tabletAlias)
	if err := wr.TopoServer().UpdateTabletFields(ctx, tabletAlias, func(tablet *topo.Tablet) error {
		if tablet.Tags == nil {
			tablet.Tags = make(map[string]string)
		}
		tablet.Tags["worker"] = ourURL
		return nil
	}); err != nil {
		return topo.TabletAlias{}, err
	}
	// we remove the tag *before* calling ChangeSlaveType back, so
	// we need to record this tag change after the change slave
	// type change in the cleaner.
	defer wrangler.RecordTabletTagAction(cleaner, tabletAlias, "worker", "")

	wr.Logger().Infof("Changing tablet %v to 'checker'", tabletAlias)
	shortCtx, cancel := context.WithTimeout(ctx, *remoteActionsTimeout)
	err = wr.ChangeType(shortCtx, tabletAlias, topo.TYPE_WORKER, false /*force*/)
	cancel()
	if err != nil {
		return topo.TabletAlias{}, err
	}

	// Record a clean-up action to take the tablet back to rdonly.
	// We will alter this one later on and let the tablet go back to
	// 'spare' if we have stopped replication for too long on it.
	wrangler.RecordChangeSlaveTypeAction(cleaner, tabletAlias, topo.TYPE_RDONLY)
	return tabletAlias, nil
}

func init() {
	rand.Seed(time.Now().UnixNano())
}
