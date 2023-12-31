package main

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	libp2pquic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/libp2p/go-libp2p/p2p/transport/quicreuse"
	"github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	"golang.org/x/sync/errgroup"

	"github.com/testground/sdk-go/network"
	"github.com/testground/sdk-go/ptypes"
	"github.com/testground/sdk-go/run"
	"github.com/testground/sdk-go/runtime"
	tgsync "github.com/testground/sdk-go/sync"
)

// Create a new libp2p host
func createHost(ctx context.Context, quic bool) (host.Host, error) {
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, 256)
	if err != nil {
		return nil, err
	}

	// Don't listen yet, we need to set up networking first
	if !quic {
		return libp2p.New(libp2p.Identity(priv), libp2p.NoListenAddrs)
	} else {
		return libp2p.New(libp2p.Identity(priv), libp2p.NoListenAddrs, libp2p.QUICReuse(quicreuse.NewConnManager), libp2p.Transport(libp2pquic.NewTransport))
	}
}

// setupNetwork instructs the sidecar (if enabled) to setup the network for this
// test case.
func setupNetwork(ctx context.Context, runenv *runtime.RunEnv, netclient *network.Client, latencyMin int, latencyMax int, bandwidth int) (*network.Config, error) {
	if !runenv.TestSidecar {
		return nil, nil
	}

	// Wait for the network to be initialized.
	runenv.RecordMessage("Waiting for network initialization")
	err := netclient.WaitNetworkInitialized(ctx)
	if err != nil {
		return nil, err
	}
	runenv.RecordMessage("Network init complete")

	lat := rand.Intn(latencyMax-latencyMin) + latencyMin

	bw := uint64(bandwidth) * 1000 * 1000

	runenv.RecordMessage("Network params %d %d", lat, bw)

	config := &network.Config{
		Network: "default",
		Enable:  true,
		Default: network.LinkShape{
			Latency:   time.Duration(lat) * time.Millisecond,
			Bandwidth: bw, //Equivalent to 100Mps
		},
		CallbackState: "network-configured",
		RoutingPolicy: network.DenyAll,
	}

	// random delay to avoid overloading weave (we hope)
	delay := time.Duration(rand.Intn(1000)) * time.Millisecond
	<-time.After(delay)
	err = netclient.ConfigureNetwork(ctx, config)
	if err != nil {
		return nil, err
	}

	return config, nil
}

// Listen on the address in the testground data network
func listenAddrs(netclient *network.Client, quic bool) []multiaddr.Multiaddr {
	ip, err := netclient.GetDataNetworkIP()
	if err == network.ErrNoTrafficShaping {
		ip = net.ParseIP("0.0.0.0")
	} else if err != nil {
		panic(fmt.Errorf("error getting data network addr: %s", err))
	}

	dataAddr, err := manet.FromIP(ip)
	if err != nil {
		panic(fmt.Errorf("could not convert IP to multiaddr; ip=%s, err=%s", ip, err))
	}

	// add /tcp/0 to auto select TCP listen port
	if quic {
		listenAddr := dataAddr.Encapsulate(multiaddr.StringCast("/udp/9000/quic-v1"))
		return []multiaddr.Multiaddr{listenAddr}
	} else {
		listenAddr := dataAddr.Encapsulate(multiaddr.StringCast("/tcp/0"))
		return []multiaddr.Multiaddr{listenAddr}
	}
}

// Called when nodes are ready to start the run, and are waiting for all other nodes to be ready
func waitForReadyState(ctx context.Context, runenv *runtime.RunEnv, client tgsync.Client) error {
	// Set a state barrier.

	state := tgsync.State("ready")
	doneCh := client.MustBarrier(ctx, state, runenv.TestInstanceCount).C

	// Signal we've entered the state.
	_, err := client.SignalEntry(ctx, state)
	if err != nil {
		return err
	}

	// Wait until all others have signalled.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-doneCh:
		if err != nil {
			return err
		}
	}

	return nil
}

func test(runenv *runtime.RunEnv, initCtx *run.InitContext) error {

	params := parseParams(runenv)

	setup := params.setup
	warmup := params.warmup
	cooldown := params.cooldown
	runTime := params.runtime
	totalTime := setup + runTime + warmup + cooldown

	ctx, cancel := context.WithTimeout(context.Background(), totalTime)
	defer cancel()

	runenv.RecordMessage("before sync.MustBoundClient")

	client := tgsync.MustBoundClient(ctx, runenv)
	defer client.Close()

	runenv.RecordMessage("after sync.MustBoundClient")

	//client := initCtx.SyncClient
	//netclient := initCtx.NetClient
	netclient := network.NewClient(client, runenv)

	// Create the hosts, but don't listen yet (we need to set up the data
	// network before listening)

	h, err := createHost(ctx, params.netParams.quic)
	if err != nil {
		return err
	}

	peers := tgsync.NewTopic("nodes", &peer.AddrInfo{})

	// Get sequence number within a node type (eg honest-1, honest-2, etc)
	// signal entry in the 'enrolled' state, and obtain a sequence number.
	seq, err := client.Publish(ctx, peers, host.InfoFromHost(h))

	if err != nil {
		return fmt.Errorf("failed to write peer subtree in sync service: %w", err)
	}

	runenv.RecordMessage("before netclient.MustConfigureNetwork")

	config, err := setupNetwork(ctx, runenv, netclient, params.netParams.latency, params.netParams.latencyMax, params.netParams.bandwidthMB)
	if err != nil {
		return fmt.Errorf("Failed to set up network: %w", err)
	}

	netclient.MustWaitNetworkInitialized(ctx)
	runenv.RecordMessage("my sequence ID: %d %s", seq, h.ID())

	peerSubscriber := NewPeerSubscriber(ctx, runenv, client, runenv.TestInstanceCount)

	var topology Topology
	topology = RandomTopology{
		Count: 2}

	discovery, err := NewSyncDiscovery(h, seq, runenv, peerSubscriber, topology)

	if err != nil {
		return fmt.Errorf("error creating discovery service: %w", err)
	}

	// Listen for incoming connections
	laddr := listenAddrs(netclient, params.netParams.quic)
	runenv.RecordMessage("listening on %s", laddr)
	if err = h.Network().Listen(laddr...); err != nil {
		runenv.RecordMessage("Error listening")
		return nil
	}

	id := host.InfoFromHost(h).ID
	runenv.RecordMessage("Host peer ID: %s, seq %d,  addrs: %v",
		id.Loggable(), seq, h.Addrs())

	err = discovery.registerAndWait(ctx)

	runenv.RecordMessage("Peers discovered %d", len(discovery.allPeers))
	if err != nil {
		runenv.RecordMessage("Failing register and wait")
		return fmt.Errorf("error waiting for discovery service: %s", err)
	}

	blocks_second := params.blocks_second
	block_size := params.block_size
	rate := ptypes.Rate{Quantity: float64(blocks_second), Interval: time.Second}
	topic := TopicConfig{Id: "block_channel", MessageRate: rate, MessageSize: ptypes.Size(block_size)}
	var topics = make([]TopicConfig, 0)
	topics = append(topics, topic)

	var pub bool
	if seq == 1 {
		pub = true
	} else {
		pub = false
	}
	tracerOut := fmt.Sprintf("%s%ctracer-output-%d", runenv.TestOutputsPath, os.PathSeparator, seq)
	tracer, err := NewTestTracer(tracerOut, h.ID(), true)

	nodeFailing := false

	if seq == int64(params.node_failing) {
		nodeFailing = true
		runenv.RecordMessage("Enabling failure for node %d !!!!!!!!!!!!!!!!!!!!!!!!!!!!!!", seq)
	}

	cfg := NodeConfig{
		Publisher:               pub,
		FloodPublishing:         false,
		PeerScoreParams:         params.scoreParams,
		OverlayParams:           params.overlayParams,
		FailureDuration:         params.node_failure_time,
		Failure:                 nodeFailing,
		Topics:                  topics,
		Tracer:                  tracer,
		Seq:                     seq,
		Warmup:                  params.warmup,
		Cooldown:                params.cooldown,
		Heartbeat:               params.heartbeat,
		ValidateQueueSize:       params.validateQueueSize,
		OutboundQueueSize:       params.outboundQueueSize,
		OpportunisticGraftTicks: params.opportunisticGraftTicks,
	}

	p, err := createPubSubNode(ctx, runenv, seq, h, discovery, netclient, config, cfg)
	if err != nil {
		runenv.RecordMessage("Failing create pubsub npde")
		return fmt.Errorf("error waiting for discovery service: %s", err)
	}

	if err := waitForReadyState(ctx, runenv, client); err != nil {
		return err
	}

	errgrp, ctx := errgroup.WithContext(ctx)

	errgrp.Go(func() (err error) {
		p.Run(runTime)

		runenv.RecordMessage("Host peer ID: %s, seq %d, addrs: %v", id, seq, h.Addrs())
		if err2 := tracer.Stop(); err2 != nil {
			runenv.RecordMessage("error stopping test tracer: %s", err2)
		}
		return
	})

	return errgrp.Wait()

}
