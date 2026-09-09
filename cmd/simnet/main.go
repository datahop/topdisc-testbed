// Command simnet-testbed runs an in-process discv5 / DISC-NG testbed using
// github.com/marcopolo/simnet for simulated UDP transport.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/marcopolo/simnet"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: simnet <config.yaml> | simnet reference")
		os.Exit(2)
	}
	if os.Args[1] == "reference" {
		printReference()
		return
	}
	cfg, err := loadConfig(os.Args[1])
	if err != nil {
		fatalf("%v", err)
	}
	runDir, err := prepareRun(&cfg)
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("run directory: %s\n", runDir)
	defer flushLog()
	nodes := &cfg.Scenario.Population.Nodes
	numTopics := &cfg.Scenario.Population.Topics
	allRegister := &cfg.Scenario.Population.AllRegister
	registerFrac := &cfg.Scenario.Population.RegisterFrac
	zipfS := &cfg.Scenario.Population.ZipfS
	commonTopicFlag := &cfg.Scenario.Population.CommonTopic
	seed := &cfg.Scenario.Population.Seed
	legacyFrac := &cfg.Scenario.Population.LegacyFrac
	vanillaFrac := &cfg.Scenario.Population.VanillaFrac
	latencyMs := &cfg.Scenario.Network.LatencyMs
	bandwidthMibps := &cfg.Scenario.Network.BandwidthMibps
	linkBuf := &cfg.Testbed.Simulator.LinkBuf
	linkNoAQM := &cfg.Testbed.Simulator.LinkNoAqm
	routerBuf := &cfg.Testbed.Simulator.RouterBuf
	routerShards := &cfg.Testbed.Simulator.RouterShards
	spawnDelay := &cfg.Testbed.Harness.SpawnDelay
	maxBootnodes := &cfg.Testbed.Harness.MaxBootnodes
	bootstrapWait := &cfg.Scenario.Phases.BootstrapWait
	registerStagger := &cfg.Scenario.Phases.RegisterStagger
	registerWait := &cfg.Scenario.Phases.RegisterWait
	searchStagger := &cfg.Scenario.Phases.SearchStagger
	searchTimeout := &cfg.Scenario.Phases.SearchTimeout
	refreshInterval := &cfg.Scenario.Phases.RefreshInterval
	adLifetime := &cfg.Scenario.Topic.AdLifetime
	adCacheSize := &cfg.Scenario.Topic.AdCacheSize
	regAttemptTimeout := &cfg.Scenario.Topic.RegAttemptTimeout
	searchBucketSize := &cfg.Scenario.Topic.SearchBucketSize
	topicNodesLimit := &cfg.Scenario.Topic.TopicNodesLimit
	auxNodesLimit := &cfg.Scenario.Topic.AuxNodesLimit
	nodesPerSourceBucket := &cfg.Scenario.Topic.NodesPerSourceBucket
	removeOnExpiryFlag := &cfg.Scenario.Topic.RemoveOnExpiry
	regProbePeriod := &cfg.Testbed.Harness.RegProbePeriod
	searchModel := &cfg.Scenario.Search.Model
	searchRequestDelay := &cfg.Scenario.Search.RequestDelay
	searchRequestTimeout := &cfg.Scenario.Search.RequestTimeout
	searchPauseMax := &cfg.Testbed.Harness.SearchPauseMax
	searchPauseNovelOnly := &cfg.Testbed.Harness.SearchPauseNovelOnly
	searchTargetCount := &cfg.Scenario.Search.TargetCount
	connModel := &cfg.Scenario.ConnModel.Enabled
	connMaxPeers := &cfg.Scenario.ConnModel.MaxPeers
	connDialRatio := &cfg.Scenario.ConnModel.DialRatio
	connRedialWait := &cfg.Scenario.ConnModel.RedialWait
	sessionChurn := &cfg.Scenario.SessionChurn.Enabled
	sessionChurnGap := &cfg.Scenario.SessionChurn.Gap
	disconnectInterval := &cfg.Scenario.Disconnect.Interval
	disconnectFrac := &cfg.Scenario.Disconnect.Frac
	churnInterval := &cfg.Scenario.Churn.Interval
	churnFrac := &cfg.Scenario.Churn.Frac
	churnMode := &cfg.Scenario.Churn.Mode
	metricsOut := &cfg.Testbed.Traces.Metrics
	overheadOutFlag := &cfg.Testbed.Traces.Overhead
	overheadSeriesOutFlag := &cfg.Testbed.Traces.OverheadSeries
	overheadSeriesPeriod := &cfg.Testbed.Traces.OverheadSeriesPeriod
	reachOutFlag := &cfg.Testbed.Traces.Reach
	snapshotDirFlag := &cfg.Testbed.Traces.SnapshotDir
	checkpointInterval := &cfg.Testbed.Traces.CheckpointInterval
	abortOnDrop := &cfg.Testbed.Safety.AbortOnDrop
	commonTopicMode = *commonTopicFlag
	nodeRemoveOnExpiry = *removeOnExpiryFlag
	reachOut = *reachOutFlag
	if reachOut != "" {
		discover.EnableReach()
	}
	if *overheadOutFlag != "" {
		discover.EnableTQRcv()
		discover.EnableWireStats()
	}
	if *overheadSeriesOutFlag != "" {
		discover.EnableWireStats()
		discover.EnableWaitStats()
	}
	snapshotDir = *snapshotDirFlag
	nodeSearchBucketSize = *searchBucketSize
	nodeAdCacheSize = *adCacheSize
	nodeTopicNodesLimit = *topicNodesLimit
	nodeAuxNodesLimit = *auxNodesLimit
	nodeRegAttemptTimeout = *regAttemptTimeout
	nodeNodesPerSourceBucket = *nodesPerSourceBucket

	// Absolute watchdog: guarantee the process exits even if the workload or
	// teardown wedges. The discv5 search-shutdown path can deadlock when a
	// heavily- or fully-churned network leaves searcher goroutines stuck, and
	// the post-teardown watchdog below only arms after the workload returns —
	// so it cannot help if the workload itself hangs. This one is armed up
	// front. It must clear every healthy-run delay before search even starts —
	// the per-node spawn and register staggers (spawnDelay×N, registerStagger×N
	// are minutes at 10k), plus bootstrap-wait, register-wait and the full
	// search-timeout — then an 8-minute grace for teardown.
	n := time.Duration(*nodes)
	hardCap := n*(*spawnDelay) + *bootstrapWait + n*(*registerStagger) + *registerWait + *searchTimeout + 20*time.Minute
	if *churnInterval > 0 {
		// Churn spawns replacement nodes while the search phase runs, which is
		// far slower than the steady-state phases the budget above assumes.
		hardCap += 20 * time.Minute
	}
	go func() {
		time.Sleep(hardCap)
		fmt.Printf("absolute watchdog (%s) expired; force-exiting\n", hardCap)
		// Flush what we can, but never let the flush prevent the exit: the
		// watchdog exists precisely for runs whose normal paths are stuck.
		if watchdogDump != nil {
			flushed := make(chan struct{})
			go func() { defer close(flushed); watchdogDump() }()
			select {
			case <-flushed:
			case <-time.After(2 * time.Minute):
				fmt.Println("watchdog: dump did not finish in time; exiting anyway")
			}
		}
		os.Exit(0)
	}()

	fmt.Printf("simnet-testbed: spawning %d nodes (latency=%dms, bw=%dMibps)\n",
		*nodes, *latencyMs, *bandwidthMibps)
	fmt.Println(cfg.paramsLine())

	sim := &simnet.Simnet{
		LatencyFunc:      simnet.StaticLatency(time.Duration(*latencyMs) * time.Millisecond),
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		RouterShardCount: *routerShards,
		RouterBufferSize: *routerBuf,
	}
	settings := simnet.NodeBiDiLinkSettings{
		Downlink: simnet.LinkSettings{BitsPerSecond: *bandwidthMibps * simnet.Mibps, BufferSize: *linkBuf, NoAQM: *linkNoAQM},
		Uplink:   simnet.LinkSettings{BitsPerSecond: *bandwidthMibps * simnet.Mibps, BufferSize: *linkBuf, NoAQM: *linkNoAQM},
	}

	legacySet := pickLegacySet(*nodes, *legacyFrac, *seed)

	// Start the simnet BEFORE spawning nodes, so each node's discv5 stack
	// sends its bootstrap packets into an already-running network instead
	// of queueing them in pre-Start buffers and releasing all at once when
	// Start fires. Combined with -spawn-delay, this spreads the bootstrap
	// burst across the spawn window instead of concentrating it at t=0.
	sim.Start()
	defer sim.Close()

	// The overhead sampler reads the node registry, so it covers every workload
	// path (including the mixed-binary one below, which spawns its own nodes)
	// and picks up mid-run churn joiners.
	var ohSeries *overheadSeries
	if *overheadSeriesOutFlag != "" {
		ohSeries = startOverheadSeries(*overheadSeriesPeriod, time.Now())
	}
	// Both the normal teardown path and the watchdog goroutine reach these, so
	// each dump runs at most once.
	var overheadOnce sync.Once
	dumpOverheadNow := func(path string) {
		nodes := liveNodeRecs()
		tqByIdx := make(map[int]int64, len(nodes))
		idByIdx := make(map[int]string, len(nodes))
		for _, nr := range nodes {
			tqByIdx[nr.idx] = discover.TopicQueryRcvCount(nr.ln.ID())
			idByIdx[nr.idx] = nr.ln.ID().String()
		}
		dumpOverhead(path, tqByIdx, idByIdx)
		fmt.Printf("overhead written to: %s\n", path)
	}
	dumpOverheadIfSet = func() {
		if *overheadOutFlag == "" {
			return
		}
		overheadOnce.Do(func() { dumpOverheadNow(*overheadOutFlag) })
	}
	var dumpOnce sync.Once
	dumpSeries := func() {
		if ohSeries == nil {
			return
		}
		// Reached from both the normal teardown path and the watchdog goroutine.
		dumpOnce.Do(func() {
			ohSeries.dump(*overheadSeriesOutFlag)
			fmt.Printf("overhead series written to: %s\n", *overheadSeriesOutFlag)
		})
	}
	// The absolute watchdog can fire mid-workload on a slow run; make sure the
	// series still reaches disk in that case.
	// The watchdog must flush every dump, not just the series: at 10k the
	// teardown regularly outlasts the budget, and losing the per-node and reach
	// dumps costs measurements the run cannot cheaply reproduce.
	watchdogDump = func() {
		dumpSeries()
		dumpOverheadIfSet()
	}

	// Mixed-binary interop workload: a fraction of nodes run the real stock
	// upstream geth v1.17.3 discv5 stack as substrate; the rest run TopDisc.
	// This path has its own spawn/teardown (two stacks) and bypasses the normal
	// single-stack path below.
	if *vanillaFrac > 0 {
		monitorStop := make(chan struct{})
		monitorDone := make(chan struct{})
		go monitorBuffers(sim, *abortOnDrop, monitorStop, monitorDone)
		pacing := searchPacing{Stagger: *searchStagger, MaxPause: *searchPauseMax, PauseNovelOnly: *searchPauseNovelOnly, TargetCount: *searchTargetCount, Checkpoint: *checkpointInterval, RedialWait: *connRedialWait, Model: *searchModel, RequestDelay: *searchRequestDelay, RequestTimeout: *searchRequestTimeout}
		runVanillaInterop(sim, settings, *nodes, *vanillaFrac, *numTopics, *zipfS, *seed,
			*bootstrapWait, *registerWait, *searchTimeout, *regProbePeriod, *registerStagger, *refreshInterval,
			*maxBootnodes, *spawnDelay, *metricsOut, pacing)
		close(monitorStop)
		<-monitorDone
		dumpSeries()
		dumpOverheadIfSet()
		fmt.Println("teardown complete")
		return
	}

	all := spawnNodes(sim, settings, *nodes, legacySet, *maxBootnodes, *spawnDelay, *refreshInterval, *adLifetime)
	defer func() {
		// Parallelize disc.Close across nodes. Sequential close of N
		// nodes takes O(N × per-node-shutdown) which becomes minutes at
		// 5k+ nodes — each Close waits for that node's dispatch
		// goroutine to drain. Fanning out lets them all shut down in
		// parallel, bounded by the Go scheduler.
		var wg sync.WaitGroup
		wg.Add(len(all))
		for _, n := range all {
			go func(n nodeRec) {
				defer wg.Done()
				n.disc.Close()
			}(n)
		}
		wg.Wait()
	}()

	// Periodic buffer-occupancy monitor. Flags when the router or any
	// link driver is approaching saturation, which indicates the shard /
	// buffer fix is being overwhelmed and senders are about to block.
	monitorStop := make(chan struct{})
	monitorDone := make(chan struct{})
	go monitorBuffers(sim, *abortOnDrop, monitorStop, monitorDone)
	defer func() {
		close(monitorStop)
		<-monitorDone
	}()

	fmt.Printf("simnet up; bootstrap-wait=%s\n", *bootstrapWait)
	time.Sleep(*bootstrapWait)

	pacing := searchPacing{
		Stagger:        *searchStagger,
		MaxPause:       *searchPauseMax,
		PauseNovelOnly: *searchPauseNovelOnly,
		TargetCount:    *searchTargetCount,
		Checkpoint:     *checkpointInterval,
		RedialWait:     *connRedialWait,
		Model:          *searchModel,
		RequestDelay:   *searchRequestDelay,
		RequestTimeout: *searchRequestTimeout,
	}
	if *connModel {
		pacing.Conns = newConnTable(all, *connMaxPeers, *connDialRatio)
		defer pacing.Conns.report()
		pacing.Resumable = *disconnectInterval > 0 || *churnInterval > 0 || *sessionChurn
		if *sessionChurn {
			scStop := make(chan struct{})
			scDone := make(chan struct{})
			go runSessionChurn(pacing.Conns, *searchTimeout, *sessionChurnGap, cfg.Scenario.SessionChurn.AlwaysOnFrac, cfg.Scenario.SessionChurn.Scale, *seed, scStop, scDone)
			defer func() { close(scStop); <-scDone }()
		}
		if *disconnectInterval > 0 {
			dcStop := make(chan struct{})
			dcDone := make(chan struct{})
			go runDisconnectDriver(pacing.Conns, *disconnectInterval, *disconnectFrac, *seed, dcStop, dcDone)
			defer func() { close(dcStop); <-dcDone }()
		}
	}

	switch {
	case *churnInterval > 0:
		runChurnWorkload(sim, settings, *maxBootnodes, *refreshInterval, all, *numTopics, *zipfS, *seed, *registerWait, *searchTimeout, *regProbePeriod, *registerStagger,
			churnParams{Interval: *churnInterval, Frac: *churnFrac, SteadyState: *churnMode == "steadystate"}, *metricsOut, pacing)
	case *allRegister:
		nt := *numTopics
		if nt < 1 {
			nt = 1
		}
		runMultiTopicWorkload(all, nt, *zipfS, *seed, *registerWait, *searchTimeout, *regProbePeriod, *registerStagger, *metricsOut, pacing)
	case *legacyFrac > 0 && *numTopics <= 1:
		runDiscNGValidationWorkload(all, *registerWait, *searchTimeout, *metricsOut)
	case *numTopics <= 1:
		runSingleTopicWorkload(all, *registerWait, *searchTimeout, *registerFrac, *metricsOut)
	default:
		// runMultiTopicWorkload skips nodes with n.legacy=true (they
		// stay as passive Discv5 peers and only contribute to the
		// routing-table substrate).
		runMultiTopicWorkload(all, *numTopics, *zipfS, *seed, *registerWait, *searchTimeout, *regProbePeriod, *registerStagger, *metricsOut, pacing)
	}
	dumpSeries()
	dumpOverheadIfSet()
	fmt.Println("teardown complete")

	// Watchdog: the deferred cleanup below (monitor stop, parallel
	// disc.Close, sim.Close) can hang at scale when one or more UDPv5
	// dispatchers get stuck. Metrics are already on disk by this
	// point, so any time spent here is pure overhead. Force-exit if
	// cleanup does not finish in 30s so the process does not sit in
	// futex_wait indefinitely.
	go func() {
		time.Sleep(30 * time.Second)
		fmt.Println("teardown grace expired (30s); force-exiting")
		os.Exit(0)
	}()
}

// watchdogDump, when set, flushes the overhead series before the absolute
// watchdog force-exits, so a slow run still yields its traffic data.
var watchdogDump func()

// dumpOverheadIfSet writes the per-node overhead dump; it is assigned in main
// so both the single-stack and mixed-binary paths can flush it exactly once.
var dumpOverheadIfSet = func() {}
