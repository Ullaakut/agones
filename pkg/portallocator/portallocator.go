// Copyright 2018 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package portallocator

import (
	"context"
	"sort"
	"sync"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"agones.dev/agones/pkg/client/informers/externalversions"
	listerv1 "agones.dev/agones/pkg/client/listers/agones/v1"
	"agones.dev/agones/pkg/util/runtime"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	corelisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// Interface manages the dynamic port allocation strategy.
//
// The portallocator does not currently support mixing static portAllocations (or any pods with defined HostPort)
// within the dynamic port range other than the ones it coordinates.
type Interface interface {
	// Run sets up the current state of port allocations and
	// starts tracking Pod and Node changes
	Run(ctx context.Context) error

	// Allocate assigns a port to the GameServer and returns it.
	Allocate(gs *agonesv1.GameServer) *agonesv1.GameServer

	// DeAllocate marks the given ports as no longer allocated
	DeAllocate(gs *agonesv1.GameServer)
}

// PortPool is a named count of ports for allocate from.
type PortPool struct {
	// Name of the pool.
	Name string
	// Min is the lowest port number for that pool.
	Min int32
	// Max is the lowest port number for that pool.
	Max int32
}

// A set of port allocations for a node
type portAllocation map[int32]bool

// A set of node allocations for a pool.
type poolAllocation map[string]portAllocation

//nolint:govet // ignore fieldalignment, singleton
type portAllocator struct {
	logger             *logrus.Entry
	mutex              sync.RWMutex
	portPools          []PortPool
	portAllocations    []poolAllocation // []map[poolName]map[port]bool
	gameServerRegistry map[types.UID]bool
	gameServerSynced   cache.InformerSynced
	gameServerLister   listerv1.GameServerLister
	gameServerInformer cache.SharedIndexInformer
	nodeSynced         cache.InformerSynced
	nodeLister         corelisterv1.NodeLister
	nodeInformer       cache.SharedIndexInformer
}

// New returns a new dynamic port allocator. minPort and maxPort are the
// top and bottom portAllocations that can be allocated in the range for
// the game servers.
func New(pools []PortPool,
	kubeInformerFactory informers.SharedInformerFactory,
	agonesInformerFactory externalversions.SharedInformerFactory) Interface {
	return newAllocator(pools, kubeInformerFactory, agonesInformerFactory)
}

func newAllocator(pools []PortPool,
	kubeInformerFactory informers.SharedInformerFactory,
	agonesInformerFactory externalversions.SharedInformerFactory) *portAllocator {
	v1 := kubeInformerFactory.Core().V1()
	nodes := v1.Nodes()
	gameServers := agonesInformerFactory.Agones().V1().GameServers()

	pa := &portAllocator{
		mutex:              sync.RWMutex{},
		portPools:          pools,
		gameServerRegistry: map[types.UID]bool{},
		gameServerSynced:   gameServers.Informer().HasSynced,
		gameServerLister:   gameServers.Lister(),
		gameServerInformer: gameServers.Informer(),
		nodeLister:         nodes.Lister(),
		nodeInformer:       nodes.Informer(),
		nodeSynced:         nodes.Informer().HasSynced,
	}
	pa.logger = runtime.NewLoggerWithType(pa)

	pa.gameServerInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: pa.syncDeleteGameServer,
	})

	for _, pool := range pools {
		pa.logger.WithField("poolName", pool.Name).WithField("min", pool.Min).WithField("max", pool.Max).Debug("Using port pool")
	}
	pa.logger.Debug("Starting")
	return pa
}

// Run sets up the current state of port allocations and
// starts tracking Pod and Node changes
func (pa *portAllocator) Run(ctx context.Context) error {
	pa.logger.Debug("Running")

	if !cache.WaitForCacheSync(ctx.Done(), pa.gameServerSynced, pa.nodeSynced) {
		return errors.New("failed to wait for caches to sync")
	}

	// on run, let's make sure we start with a perfect slate straight away
	if err := pa.syncAll(); err != nil {
		return errors.Wrap(err, "error performing initial sync")
	}

	return nil
}

// Allocate assigns a port to the GameServer and returns it.
func (pa *portAllocator) Allocate(gs *agonesv1.GameServer) *agonesv1.GameServer {
	pa.mutex.Lock()
	defer pa.mutex.Unlock()

	type pn struct {
		pa   portAllocation
		port int32
	}

	// we only want this to be called inside the mutex lock
	// so let's define the function here so it can never be called elsewhere.
	// Also the return gives an escape from the double loop
	findOpenPorts := func(pool string, count int) []pn {
		if count <= 0 {
			return []pn{}
		}

		var ports []pn
		for _, allocs := range pa.portAllocations {
			for p, taken := range allocs[pool] {
				if taken {
					continue
				}

				ports = append(ports, pn{pa: allocs[pool], port: p})

				// Once a pool has enough ports allocated, move on to next pool.
				if len(ports) == count {
					break
				}
			}
		}
		return ports
	}

	// this allows us to do recursion, within the mutex lock
	var allocate func(gs *agonesv1.GameServer) *agonesv1.GameServer
	allocate = func(gs *agonesv1.GameServer) *agonesv1.GameServer {
		pools := gs.CountPorts(func(policy agonesv1.PortPolicy) bool {
			return policy == agonesv1.Dynamic || policy == agonesv1.Passthrough
		})
		allocations := make(map[string][]pn, len(pools))
		var want, got int
		for name, count := range pools {
			allocations[name] = findOpenPorts(name, count)
			want += count
			got += len(allocations[name])
		}

		if got >= want {
			pa.gameServerRegistry[gs.ObjectMeta.UID] = true

			var extraPorts []agonesv1.GameServerPort

			for i, p := range gs.Spec.Ports {
				if p.PortPolicy != agonesv1.Dynamic && p.PortPolicy != agonesv1.Passthrough {
					continue
				}

				if _, ok := allocations[p.Pool]; !ok {
					pa.logger.Error("Unknown pool name found on game server port, using generic pool instead")
				}
				// pop off allocation for matching pool.
				var a pn
				a, allocations[p.Pool] = allocations[p.Pool][0], allocations[p.Pool][1:]

				a.pa[a.port] = true
				gs.Spec.Ports[i].HostPort = a.port

				if p.PortPolicy == agonesv1.Passthrough {
					gs.Spec.Ports[i].ContainerPort = a.port
				}

				// create a port for TCP when using TCPUDP protocol
				if p.Protocol == agonesv1.ProtocolTCPUDP {
					var duplicate = p
					duplicate.HostPort = a.port

					if duplicate.PortPolicy == agonesv1.Passthrough {
						duplicate.ContainerPort = a.port
					}

					extraPorts = append(extraPorts, duplicate)

					gs.Spec.Ports[i].Name = p.Name + "-tcp"
					gs.Spec.Ports[i].Protocol = corev1.ProtocolTCP
				}
			}

			// create the UDP port when using TCPUDP protocol
			for _, p := range extraPorts {
				p.Name += "-udp"
				p.Protocol = corev1.ProtocolUDP
				gs.Spec.Ports = append(gs.Spec.Ports, p)
			}

			return gs
		}

		// if we get here, we ran out of ports. Add a node, and try again.
		// this is important, because to autoscale scale up, we create GameServers that
		// can't be scheduled on the current set of nodes, so we need to be sure
		// there are always ports available to be allocated.
		pa.portAllocations = append(pa.portAllocations, pa.newPoolAllocation())

		return allocate(gs)
	}

	return allocate(gs)
}

// DeAllocate marks the given ports as no longer allocated
func (pa *portAllocator) DeAllocate(gs *agonesv1.GameServer) {
	// skip if it wasn't previously allocated

	found := func() bool {
		pa.mutex.RLock()
		defer pa.mutex.RUnlock()
		if _, ok := pa.gameServerRegistry[gs.ObjectMeta.UID]; ok {
			return true
		}
		return false
	}

	if !found() {
		pa.logger.WithField("gs", gs.ObjectMeta.Name).
			Debug("Did not allocate this GameServer. Ignoring for DeAllocation")
		return
	}

	pa.mutex.Lock()
	defer pa.mutex.Unlock()
	for _, p := range gs.Spec.Ports {
		match := false
		for _, pool := range pa.portPools {
			if p.HostPort >= pool.Min && p.HostPort <= pool.Max {
				match = true
				break
			}
		}
		if !match {
			continue
		}

		pa.portAllocations = setPortAllocation(p.HostPort, pa.portAllocations, false)
	}

	delete(pa.gameServerRegistry, gs.ObjectMeta.UID)
}

// syncDeleteGameServer when a GameServer Pod is deleted
// make the HostPort available
func (pa *portAllocator) syncDeleteGameServer(object interface{}) {
	if gs, ok := object.(*agonesv1.GameServer); ok {
		pa.logger.WithField("gs", gs).Debug("Syncing deleted GameServer")
		pa.DeAllocate(gs)
	}
}

// syncAll syncs the pod, node and gameserver caches then
// traverses all Nodes in the cluster and all looks at GameServers
// and Terminating Pods values make sure those
// portAllocations are marked as taken.
// Locks the mutex while doing this.
// This is basically a stop the world Garbage Collection on port allocations, but it only happens on startup.
func (pa *portAllocator) syncAll() error {
	pa.mutex.Lock()
	defer pa.mutex.Unlock()

	pa.logger.Debug("Resetting Port Allocation")

	nodes, err := pa.nodeLister.List(labels.Everything())
	if err != nil {
		return errors.Wrap(err, "error listing all nodes")
	}

	gameservers, err := pa.gameServerLister.List(labels.Everything())
	if err != nil {
		return errors.Wrapf(err, "error listing all GameServers")
	}

	gsRegistry := map[types.UID]bool{}

	// place to put GameServer port allocations that are not ready yet/after the ready state
	allocations, nonReadyNodesPorts := pa.registerExistingGameServerPorts(gameservers, nodes, gsRegistry)

	// close off the port on the first node you find
	// we actually don't mind what node it is, since we only care
	// that there is a port open *somewhere* as the default scheduler
	// will re-route for us based on HostPort allocation
	for _, p := range nonReadyNodesPorts {
		allocations = setPortAllocation(p, allocations, true)
	}

	pa.portAllocations = allocations
	pa.gameServerRegistry = gsRegistry

	return nil
}

// registerExistingGameServerPorts registers the gameservers against gsRegistry and the ports against nodePorts.
// and returns an ordered list of portAllocations per cluster nodes, and an array of
// any GameServers allocated a port, but not yet assigned a Node will returned as an array of port values.
func (pa *portAllocator) registerExistingGameServerPorts(gameservers []*agonesv1.GameServer, nodes []*corev1.Node, gsRegistry map[types.UID]bool) ([]poolAllocation, []int32) {
	// setup blank port values
	nodePortAllocation := pa.nodePortAllocation(nodes)
	nodePortCount := make(map[string]int64, len(nodes))
	for _, n := range nodes {
		nodePortCount[n.ObjectMeta.Name] = 0
	}

	var nonReadyNodesPorts []int32

	for _, gs := range gameservers {
		for _, p := range gs.Spec.Ports {
			if p.PortPolicy != agonesv1.Dynamic && p.PortPolicy != agonesv1.Passthrough {
				continue
			}
			gsRegistry[gs.ObjectMeta.UID] = true

			var poolName string
			for _, pool := range pa.portPools {
				if pool.Min <= p.HostPort && p.HostPort <= pool.Max {
					poolName = pool.Name
					break
				}
			}

			// if the node doesn't exist, it's likely unscheduled
			_, ok := nodePortAllocation[gs.Status.NodeName]
			if gs.Status.NodeName != "" && ok {
				nodePortAllocation[gs.Status.NodeName][poolName][p.HostPort] = true
				nodePortCount[gs.Status.NodeName]++
			} else if p.HostPort != 0 {
				nonReadyNodesPorts = append(nonReadyNodesPorts, p.HostPort)
			}
		}
	}

	// make a list of the keys
	keys := make([]string, 0, len(nodePortAllocation))
	for k := range nodePortAllocation {
		keys = append(keys, k)
	}

	// sort, since this is how it would have originally been allocated across the
	// ordered []poolAllocation
	sort.Slice(keys, func(i, j int) bool {
		return nodePortCount[keys[i]] > nodePortCount[keys[j]]
	})

	// this gives us back an ordered node list
	allocations := make([]poolAllocation, len(nodePortAllocation))
	for i, k := range keys {
		allocations[i] = nodePortAllocation[k]
	}

	return allocations, nonReadyNodesPorts
}

// nodePortAllocation returns a map of port allocations all set to being available
// with a map key for each node, as well as the node registry record (since we're already looping)
func (pa *portAllocator) nodePortAllocation(nodes []*corev1.Node) map[string]poolAllocation {
	nodePorts := map[string]poolAllocation{}

	for _, n := range nodes {
		// ignore unschedulable nodes
		if !n.Spec.Unschedulable {
			nodePorts[n.Name] = pa.newPoolAllocation()
		}
	}

	return nodePorts
}

func (pa *portAllocator) newPoolAllocation() poolAllocation {
	p := make(poolAllocation, len(pa.portPools))
	for _, pool := range pa.portPools {
		ports := make(portAllocation, (pool.Max-pool.Min)+1)
		for i := pool.Min; i <= pool.Max; i++ {
			ports[i] = false
		}

		p[pool.Name] = ports
	}

	return p
}

// setPortAllocation takes a port from an all
func setPortAllocation(port int32, allocations []poolAllocation, taken bool) []poolAllocation {
	for _, pool := range allocations {
		for _, np := range pool {
			if np[port] != taken {
				np[port] = taken
				return allocations
			}
		}
	}
	return allocations
}
