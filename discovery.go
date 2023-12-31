package main

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/avast/retry-go"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	swarm "github.com/libp2p/go-libp2p/p2p/net/swarm"
	"golang.org/x/sync/errgroup"

	"github.com/testground/sdk-go/runtime"
	tgsync "github.com/testground/sdk-go/sync"
)

type NodeType string

/*const (
	NodeTypeSybil  NodeType = "sybil"
	NodeTypeGraft  NodeType = "graft"
	NodeTypeHonest NodeType = "honest"
)*/

const (
	PeerConnectTimeout = time.Second * 10
	MaxConnectRetries  = 10
)

type ConnectionsDef struct {
	Latency     time.Duration
	Connections []string
}

// SyncDiscovery uses the testground sync API to share PeerRegistrations for the
// local test peers and collect the info from all the other peers. It then allows
// you to connect the local peers to a subset of the test peers, using a Topology
// to control the peer selection.
type SyncDiscovery struct {
	h              host.Host
	runenv         *runtime.RunEnv
	peerSubscriber *PeerSubscriber
	topology       Topology
	//nodeType       NodeType
	nodeTypeSeq int64
	isPublisher bool

	// All peers in the test
	allPeers []PeerRegistration

	// The peers that this node connects to
	connectedLk sync.RWMutex
	connected   map[peer.ID]PeerRegistration
}

// A Topology filters the set of all nodes
type Topology interface {
	SelectPeers(local peer.ID, remote []PeerRegistration) []PeerRegistration
	SelectNPeers(n int, local peer.ID, remote []PeerRegistration) []PeerRegistration
}

// RandomTopology selects a subset of the total nodes at random
type RandomTopology struct {
	// Count is the number of total peers to return
	Count int
}

func (t RandomTopology) SelectPeers(local peer.ID, remote []PeerRegistration) []PeerRegistration {
	if len(remote) == 0 || t.Count == 0 {
		return []PeerRegistration{}
	}

	n := t.Count
	if n > len(remote) {
		n = len(remote)
	}

	indices := rand.Perm(len(remote))
	out := make([]PeerRegistration, n)
	for i := 0; i < n; i++ {
		out[i] = remote[indices[i]]
	}
	return out
}

func (t RandomTopology) SelectNPeers(n int, local peer.ID, remote []PeerRegistration) []PeerRegistration {
	if len(remote) == 0 || n == 0 {
		return []PeerRegistration{}
	}

	if n > len(remote) {
		n = len(remote)
	}

	indices := rand.Perm(len(remote))
	out := make([]PeerRegistration, n)
	for i := 0; i < n; i++ {
		out[i] = remote[indices[i]]
	}
	return out
}

// RandomHonestTopology is a Topology that returns a subset of all non-attack nodes
type RandomHonestTopology struct {
	// Count is the number of total peers to return
	Count int
	// PublishersOnly indicates whether to connect to publishers only or to
	// both publishers and lurkers
	PublishersOnly bool
}

func (t RandomHonestTopology) SelectPeers(local peer.ID, remote []PeerRegistration) []PeerRegistration {
	if len(remote) == 0 {
		return []PeerRegistration{}
	}

	filtered := make([]PeerRegistration, 0, len(remote))
	for _, peer := range remote {
		// Only connect to honest nodes.
		// If PublishersOnly is true, only connect to Publishers
		if /*peer.NType == NodeTypeHonest &&*/ !t.PublishersOnly || peer.IsPublisher {
			filtered = append(filtered, peer)
		}
	}

	return RandomTopology{t.Count}.SelectPeers(local, filtered)
}

// SinglePublisherTopology is a Topology that returns the first publisher node
type SinglePublisherTopology struct {
}

func (t SinglePublisherTopology) SelectPeers(local peer.ID, remote []PeerRegistration) []PeerRegistration {
	publisher := selectSinglePublisher(remote)
	if publisher != nil {
		return []PeerRegistration{*publisher}
	}
	return []PeerRegistration{}
}

// Select the publisher with the lowest sequence number and index
func selectSinglePublisher(peers []PeerRegistration) *PeerRegistration {
	lowest := int64(-1)
	var lowestp PeerRegistration
	for _, p := range peers {
		if p.IsPublisher {
			current := int64(p.NodeTypeSeq * 1000000)
			if lowest < 0 || current < lowest {
				lowest = current
				lowestp = p
			}
		}
	}
	if lowest == -1 {
		return nil
	}
	return &lowestp
}

// FixedTopology is defined by a topology file
type FixedTopology struct {
	// def contains the definition of the topology
	def *ConnectionsDef
}

func (t FixedTopology) SelectPeers(local peer.ID, remote []PeerRegistration) []PeerRegistration {
	if len(remote) == 0 {
		return []PeerRegistration{}
	}

	out := make([]PeerRegistration, 0, len(t.def.Connections))
	for _, conn := range t.def.Connections {
		parts := strings.Split(conn, "-")
		if len(parts) != 3 {
			panic(fmt.Sprintf("Badly formatted topology file"))
		}
		//nodeType := parts[0]
		nodeTypeSeq := parts[0]
		//nodeIdx := parts[2]
		for _, p := range remote {
			//if nodeType == string(p.NType) && nodeTypeSeq == strconv.Itoa(int(p.NodeTypeSeq)) {
			if nodeTypeSeq == strconv.Itoa(int(p.NodeTypeSeq)) {
				out = append(out, p)
			}
		}
	}
	return out
}

// PeerRegistration contains the addresses, sequence numbers and node type (honest / sybil / etc)
// for each peer in the test. It is shared with every other peer using the sync service.
type PeerRegistration struct {
	Info peer.AddrInfo
	//NType       NodeType
	NodeTypeSeq int64
	IsPublisher bool
}

// PeerSubscriber subscribes to peer information from all nodes in all containers.
// There is one PeerSubscriber per container (but there may be several nodes per container)
type PeerSubscriber struct {
	lk             sync.Mutex
	peers          []PeerRegistration
	runenv         *runtime.RunEnv
	client         tgsync.Client
	containerCount int
}

func NewPeerSubscriber(ctx context.Context, runenv *runtime.RunEnv, client tgsync.Client, containerCount int) *PeerSubscriber {
	return &PeerSubscriber{
		runenv:         runenv,
		client:         client,
		containerCount: containerCount,
	}
}

var PeerRegistrationTopic = tgsync.NewTopic("pubsub-test-peers", &PeerRegistration{})

// Register node information for the local node
func (ps *PeerSubscriber) register(ctx context.Context, entry PeerRegistration) error {

	//ps.runenv.RecordMessage("registering peers for %s %s %d %s \n", entry.Info, entry.NType, entry.NodeTypeSeq, entry.IsPublisher)
	if _, err := ps.client.Publish(ctx, PeerRegistrationTopic, &entry); err != nil {
		ps.runenv.RecordMessage("registering peers not publishing %w", err)
		return fmt.Errorf("failed to write to pubsub subtree in sync service: %w", err)
	}

	return nil
}

// Wait for node information from all nodes in all containers
func (ps *PeerSubscriber) waitForPeers(ctx context.Context) ([]PeerRegistration, error) {
	ps.lk.Lock()
	defer ps.lk.Unlock()

	if ps.peers != nil {
		return ps.peers, nil
	}

	// wait for all other peers to send their peer registration
	peerCh := make(chan *PeerRegistration, 16)
	ps.peers = make([]PeerRegistration, 0, ps.containerCount)

	// add a random delay before subscribing, to avoid overloading the subscriber system
	delay := time.Duration(rand.Intn(ps.containerCount)) * time.Millisecond
	if delay > time.Second {
		ps.runenv.RecordMessage("waiting for %s before subscribing", delay)
	}
	time.Sleep(delay)

	sctx, cancelSub := context.WithCancel(ctx)
	if _, err := ps.client.Subscribe(sctx, PeerRegistrationTopic, peerCh); err != nil {
		cancelSub()
		return nil, err
	}
	defer cancelSub()

	start := time.Now()
	//ps.runenv.RecordMessage("waiting for peer information from %d peers", ps.containerNodesTotal)
	for i := 0; i < ps.containerCount; i++ {
		select {
		case ai, ok := <-peerCh:
			if !ok {
				return nil, fmt.Errorf("not enough peer infos. expected %d, got %d", ps.containerCount, len(ps.peers))
			}
			ps.peers = append(ps.peers, *ai)
			ps.runenv.RecordMessage("received peer information from %d of %d peers in %s %s", len(ps.peers), ps.containerCount, time.Since(start), ai.Info.ID)

			if len(ps.peers)%500 == 0 {
				ps.runenv.RecordMessage("received peer information from %d of %d peers in %s", len(ps.peers), ps.containerCount, time.Since(start))
			}
		case <-ctx.Done():
			ps.runenv.RecordMessage("context cancelled before receiving peer information from %d peers: %s", ps.containerCount, ctx.Err())
			return nil, ctx.Err()
		}
	}

	//ps.runenv.RecordMessage("received peer information from %d peers in %s", len(ps.peers), time.Since(start))

	return ps.peers, nil
}

/*func NewSyncDiscovery(h host.Host, runenv *runtime.RunEnv, peerSubscriber *PeerSubscriber, topology Topology, nodeType NodeType, nodeTypeSeq int64, nodeIdx int, isPublisher bool) (*SyncDiscovery, error) {

	return &SyncDiscovery{
		h:              h,
		runenv:         runenv,
		peerSubscriber: peerSubscriber,
		topology:       topology,
		nodeType:       nodeType,
		nodeTypeSeq:    nodeTypeSeq,
		//nodeIdx:        nodeIdx,
		isPublisher: isPublisher,
		connected:   make(map[peer.ID]PeerRegistration),
	}, nil
}*/

func NewSyncDiscovery(h host.Host, seq int64, runenv *runtime.RunEnv, peerSubscriber *PeerSubscriber, topology Topology) (*SyncDiscovery, error) {

	return &SyncDiscovery{
		h:              h,
		runenv:         runenv,
		peerSubscriber: peerSubscriber,
		topology:       topology,
		nodeTypeSeq:    seq,
		//nodeIdx:        nodeIdx,
		connected: make(map[peer.ID]PeerRegistration),
	}, nil
}

// Registers node and waits to collect all other nodes' registrations.
func (s *SyncDiscovery) registerAndWait(ctx context.Context) error {
	// Register this node's information
	localPeer := *host.InfoFromHost(s.h)
	entry := PeerRegistration{
		Info: localPeer,
		//NType:       s.nodeType,
		NodeTypeSeq: s.nodeTypeSeq,
		//NodeIdx:     s.nodeIdx,
		IsPublisher: s.isPublisher,
	}

	s.peerSubscriber.runenv.RecordMessage("registering peers %s", entry)
	err := s.peerSubscriber.register(ctx, entry)
	if err != nil {

		return err
	}

	s.peerSubscriber.runenv.RecordMessage("waiting for peers")

	// Wait for all peers' node information
	peers, err := s.peerSubscriber.waitForPeers(ctx)
	if err != nil {
		return err
	}

	s.peerSubscriber.runenv.RecordMessage("filtering peers")

	// Filter out this node's information from all peers
	s.allPeers = make([]PeerRegistration, 0, len(peers)-1)
	for _, p := range peers {
		if p.Info.ID != localPeer.ID {
			s.allPeers = append(s.allPeers, p)
		}
	}

	s.peerSubscriber.runenv.RecordMessage("register and wait done")

	return nil
}

// Connect to all peers in the topology
func (s *SyncDiscovery) ConnectTopology(ctx context.Context, delay time.Duration) error {
	s.runenv.RecordMessage("delay connect to peers by %s", delay)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		s.runenv.RecordMessage("connecting to peers after %s", delay)
	}

	s.runenv.RecordMessage("selecting peers between %d", len(s.allPeers))

	selected := s.topology.SelectPeers(s.h.ID(), s.allPeers)

	s.runenv.RecordMessage("Connecting topology with %d nodes", len(selected))
	if len(selected) == 0 {
		panic("topology selected zero peers. so lonely!!!")
	}

	s.connectedLk.Lock()

	errgrp, ctx := errgroup.WithContext(ctx)
	for _, p := range selected {
		p := p
		if _, ok := s.connected[p.Info.ID]; !ok {
			s.connected[p.Info.ID] = p
			s.runenv.RecordMessage("%d connecting to %d\n", s.nodeTypeSeq, p.NodeTypeSeq)
			errgrp.Go(func() error {
				err := s.connectWithRetry(ctx, p.Info)
				if err != nil {
					s.runenv.RecordMessage("error connecting libp2p host: %s", err)
				}
				conns := s.h.Network().ConnsToPeer(p.Info.ID)
				for _, conn := range conns {
					s.runenv.RecordMessage("%d connected to %d. local addr: %s remote addr: %s\n",
						s.nodeTypeSeq, p.NodeTypeSeq,
						conn.LocalMultiaddr(), conn.RemoteMultiaddr())
				}
				return err
			})
		}
	}

	s.connectedLk.Unlock()

	return errgrp.Wait()
}

// Connect to all peers in the topology
func (s *SyncDiscovery) ConnectingToPeers(ctx context.Context, peers []PeerRegistration) error {

	selected := peers

	s.runenv.RecordMessage("Connecting topology with %d nodes", len(selected))
	if len(selected) == 0 {
		panic("topology selected zero peers. so lonely!!!")
	}

	s.connectedLk.Lock()

	errgrp, ctx := errgroup.WithContext(ctx)
	for _, p := range selected {
		p := p
		if _, ok := s.connected[p.Info.ID]; !ok {
			s.connected[p.Info.ID] = p
			s.runenv.RecordMessage("%d connecting to %d\n", s.nodeTypeSeq, p.NodeTypeSeq)
			errgrp.Go(func() error {
				err := s.connectWithRetry(ctx, p.Info)
				if err != nil {
					s.runenv.RecordMessage("error connecting libp2p host: %s", err)
				}
				conns := s.h.Network().ConnsToPeer(p.Info.ID)
				for _, conn := range conns {
					s.runenv.RecordMessage("%d connected to %d. local addr: %s remote addr: %s\n",
						s.nodeTypeSeq, p.NodeTypeSeq,
						conn.LocalMultiaddr(), conn.RemoteMultiaddr())
				}
				return err
			})
		}
	}

	s.connectedLk.Unlock()

	return errgrp.Wait()
}

func (s *SyncDiscovery) connectWithRetry(ctx context.Context, p peer.AddrInfo) error {
	return retry.Do(
		func() error {
			// add a random delay to each connection attempt to spread the network load
			connectDelay := time.Duration(rand.Intn(10000)) * time.Millisecond
			<-time.After(connectDelay)

			boundedCtx, cancel := context.WithTimeout(ctx, PeerConnectTimeout)
			defer cancel()
			return s.h.Connect(boundedCtx, p)
		},
		retry.Attempts(MaxConnectRetries),
		retry.OnRetry(func(n uint, err error) {
			s.runenv.RecordMessage("connection attempt #%d to %s failed: %s", n, p.ID.Loggable(), err)

			// clear the libp2p dial backoff for this peer, otherwise the swarm will ignore our
			// dial attempt and immediately return a "dial backoff" error
			if sw, ok := s.h.Network().(*swarm.Swarm); ok {
				s.runenv.RecordMessage("clearing swarm dial backoff for peer %s", p.ID.Loggable())
				sw.Backoff().Clear(p.ID)
			}
		}),
	)
}

func (s *SyncDiscovery) Connected() []PeerRegistration {
	s.connectedLk.RLock()
	defer s.connectedLk.RUnlock()

	d := make([]PeerRegistration, 0, len(s.connected))
	for _, p := range s.connected {
		d = append(d, p)
	}
	return d
}
