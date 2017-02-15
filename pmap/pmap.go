package pmap

import (
	"fmt"
	"net"
	"strings"
	"sync"
)

const (
	gfIanaPrivPortsStart = 49152
	gfPortMax            = 65535
)

// PortType represents the type of state of the port
type PortType int32

// List of states an individual port can be in
const (
	GfPmapPortFree PortType = iota
	GfPmapPortForeign
	GfPmapPortLeased
	GfPmapPortNone
	GfPmapPortBrickserver
)

type portStatus struct {
	Type       PortType
	Bricknames []string // brick muxing
	Xprt       interface{}
}

var registry = struct {
	sync.RWMutex
	BasePort  int
	LastAlloc int
	Ports     [gfPortMax]portStatus
}{}

// NOTE: Export the functions defined here only when other parts of glusterd2
//       actually starts using them.
// TODO: There is no RDMA specific handling yet.

func isPortFree(port int) bool {
	conn, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func registrySearchByPort(port int) string {

	if port > gfPortMax {
		return ""
	}

	registry.RLock()
	defer registry.RUnlock()

	if registry.Ports[port].Type == GfPmapPortBrickserver {
		// TODO:
		// This is what glusterd1's implementation returns after brick
		// multiplexing feature got in. But who's really using the
		// 'BRICKBYPORT' RPC call (calls this method) anyway ?
		return strings.Join(registry.Ports[port].Bricknames, " ")
	}

	return ""
}

func registrySearchByXprt(xprt interface{}, ptype PortType) int {
	var port int
	for p := registry.LastAlloc; p >= registry.BasePort; p-- {
		if registry.Ports[p].Xprt == nil {
			continue
		}
		if (registry.Ports[p].Xprt == xprt) && (registry.Ports[p].Type == ptype) {
			port = p
			break
		}
	}
	return port
}

func stringInSlice(query string, list []string) bool {
	for _, s := range list {
		if s == query {
			return true
		}
	}
	return false
}

// NOTE: Unlike glusterd1's implementation, the search here is not overloaded
// with delete operation. This is intentionally kept simple
func registrySearch(brickname string, ptype PortType) int {
	registry.RLock()
	defer registry.RUnlock()

	for p := registry.LastAlloc; p >= registry.BasePort; p-- {

		if len(registry.Ports[p].Bricknames) == 0 || registry.Ports[p].Type != ptype {
			continue
		}

		if stringInSlice(brickname, registry.Ports[p].Bricknames) {
			return p
		}
	}

	return 0
}

func registryAlloc(recheckForeign bool) int {
	registry.Lock()
	defer registry.Unlock()

	var port int
	for p := registry.BasePort; p <= gfPortMax; p++ {
		if registry.Ports[p].Type == GfPmapPortFree ||
			(recheckForeign && registry.Ports[p].Type == GfPmapPortForeign) {

			if isPortFree(p) {
				registry.Ports[p].Type = GfPmapPortLeased
				port = p
				break
			} else {
				// We may have an opportunity here to change
				// the port's status from GfPmapPortFree to
				// GfPmapPortForeign. Passing on for now...
			}
		}
	}

	if port > registry.LastAlloc {
		registry.LastAlloc = port
	}

	return port
}

// AssignPort allocates and returns an available port. Optionally, if an
// oldport specified for the brickpath, stale ports for the brickpath will
// be cleaned up
func AssignPort(oldport int, brickpath string) int {
	if oldport != 0 {
		registryRemove(0, brickpath, GfPmapPortBrickserver, nil)
	}
	return registryAlloc(true)
}

func registryBind(port int, brickname string, ptype PortType, xprt interface{}) {

	if port > gfPortMax {
		return
	}

	registry.Lock()
	defer registry.Unlock()

	registry.Ports[port].Type = ptype
	registry.Ports[port].Bricknames = append(registry.Ports[port].Bricknames, brickname)
	registry.Ports[port].Xprt = xprt

	if registry.LastAlloc < port {
		registry.LastAlloc = port
	}
}

// opposite of append(), fast but doesn't maintain order
func deleteFromSlice(list []string, query string) []string {

	var found bool
	var pos int
	for i, s := range list {
		if s == query {
			pos = i
			found = true
			break
		}
	}

	if found {
		// swap i'th element with the last element
		list[len(list)-1], list[pos] = list[pos], list[len(list)-1]
		return list[:len(list)-1]
	}

	return list
}

func registryRemove(port int, brickname string, ptype PortType, xprt interface{}) {
	registry.Lock()
	defer registry.Unlock()

	if port > 0 {
		if port > gfPortMax {
			return
		}
		goto REMOVE
	}

	if brickname != "" && strings.HasPrefix(brickname, "/") {
		port = registrySearch(brickname, ptype)
		if port != 0 {
			goto REMOVE
		}
	}

	if xprt != nil {
		port = registrySearchByXprt(xprt, ptype)
		if port != 0 {
			goto REMOVE
		}
	}

	goto OUT

REMOVE:

	// TODO: This code below needs some more careful attention and actual
	// testing; especially around presence/absence of Xprt in case of
	// multiplexed bricks, tierd and snapd - all of which seem to use the
	// pmap service.

	if len(registry.Ports[port].Bricknames) == 1 {
		// Bricks aren't multiplexed over the same port
		// clear the bricknames array and reset other fields
		registry.Ports[port].Bricknames = registry.Ports[port].Bricknames[:0]
		registry.Ports[port].Type = GfPmapPortFree
		registry.Ports[port].Xprt = nil
	} else {
		// Bricks are multiplexed, only remove the brick entry
		registry.Ports[port].Bricknames =
			deleteFromSlice(registry.Ports[port].Bricknames, brickname)
		if (xprt != nil) && (xprt == registry.Ports[port].Xprt) {
			registry.Ports[port].Xprt = nil
		}
	}

OUT:
	return
}

var registryInit sync.Once

func initRegistry() {
	registry.Lock()
	defer registry.Unlock()

	// TODO: When a config option by the name 'base-port'
	// becomes available, use that.
	registry.BasePort = gfIanaPrivPortsStart

	for i := registry.BasePort; i <= gfPortMax; i++ {
		if isPortFree(i) {
			registry.Ports[i].Type = GfPmapPortFree
		} else {
			registry.Ports[i].Type = GfPmapPortForeign
		}
	}
}

func init() {
	registryInit.Do(initRegistry)
}
