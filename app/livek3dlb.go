package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// livek3dlb.go — the LoadBalancer addresses a K3D cluster has handed out, for the Live view.
//
// A K3D frame's MetalLB pool is a block of the stack subnet, and every LoadBalancer Service in
// the cluster takes an address from it: the operator's own (a PXC HAProxy, a MongoDB replica
// set exposed per pod), the Expose control's, Performance Sim's per-pod companions. Which of
// those addresses are in use is what someone pointing a client, a capture or a DNS record at
// the cluster needs to know, and the cluster does not otherwise show it anywhere on the canvas.
//
// One `kubectl get svc -A` on the frame's server per poll would be an exec every 2–5 s per
// cluster, so the answer is cached per server container for liveLBTTL — addresses change only
// when a Service is created or deleted.

const (
	liveLBTTL  = 10 * time.Second
	liveLBTime = 6 * time.Second
)

// liveLB is one LoadBalancer Service with an address.
type liveLB struct {
	IP        string `json:"ip"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ports     []int  `json:"ports,omitempty"`
}

var liveLBCache = struct {
	mu sync.Mutex
	m  map[string]liveLBEntry
}{m: map[string]liveLBEntry{}}

type liveLBEntry struct {
	at  time.Time
	lbs []liveLB
}

// liveK3DLBs is, for each K3D frame of the stack whose server is running, the LoadBalancer
// Services that have an address. A frame whose server cannot be asked is left out rather than
// reported empty — "no addresses" and "could not tell" are different answers.
func (a *App) liveK3DLBs(ctx context.Context, st Stack) map[string][]liveLB {
	doc := buildDoc(st)
	out := map[string][]liveLB{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, f := range doc.Frames {
		if f.Type != "k3d" {
			continue
		}
		_, serverID, ok := a.k3dServerConfig(st.ID, doc, f.ID)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(frameID, serverID string) {
			defer wg.Done()
			key := fmt.Sprintf("%d:%s:%s", st.ID, frameID, serverID)
			liveLBCache.mu.Lock()
			e, hit := liveLBCache.m[key]
			liveLBCache.mu.Unlock()
			if !hit || time.Since(e.at) > liveLBTTL {
				c, cancel := context.WithTimeout(withEngine(ctx, a.nodeEngine(st, "k3d")), liveLBTime)
				js, err := a.kubectl(c, serverID, "get", "svc", "-A", "-o", "json")
				cancel()
				if err != nil {
					return
				}
				e = liveLBEntry{at: time.Now(), lbs: liveLBsFromServices(js)}
				liveLBCache.mu.Lock()
				liveLBCache.m[key] = e
				liveLBCache.mu.Unlock()
			}
			mu.Lock()
			out[frameID] = e.lbs
			mu.Unlock()
		}(f.ID, serverID)
	}
	wg.Wait()
	return out
}

// liveLBsFromServices picks the LoadBalancer Services with an address out of
// `kubectl get svc -A -o json`, ordered by address. Unlike k3dLBAddrsFromServices it keeps
// every namespace and every Service — the point is the whole pool, not one cluster's names.
func liveLBsFromServices(js string) []liveLB {
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Type  string `json:"type"`
				Ports []struct {
					Port int `json:"port"`
				} `json:"ports"`
			} `json:"spec"`
			Status struct {
				LoadBalancer struct {
					Ingress []struct {
						IP string `json:"ip"`
					} `json:"ingress"`
				} `json:"loadBalancer"`
			} `json:"status"`
		} `json:"items"`
	}
	out := []liveLB{}
	if json.Unmarshal([]byte(js), &list) != nil {
		return out
	}
	for _, s := range list.Items {
		if s.Spec.Type != "LoadBalancer" {
			continue
		}
		for _, in := range s.Status.LoadBalancer.Ingress {
			if in.IP == "" {
				continue
			}
			lb := liveLB{IP: in.IP, Namespace: s.Metadata.Namespace, Name: s.Metadata.Name}
			for _, p := range s.Spec.Ports {
				lb.Ports = append(lb.Ports, p.Port)
			}
			out = append(out, lb)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return ipLess(out[i].IP, out[j].IP) })
	return out
}

// ipLess orders dotted IPv4 addresses numerically (172.29.0.9 before 172.29.0.10); anything
// that is not one sorts by its text.
func ipLess(x, y string) bool {
	var a, b [4]int
	if n, _ := fmt.Sscanf(x, "%d.%d.%d.%d", &a[0], &a[1], &a[2], &a[3]); n == 4 {
		if m, _ := fmt.Sscanf(y, "%d.%d.%d.%d", &b[0], &b[1], &b[2], &b[3]); m == 4 {
			for i := range a {
				if a[i] != b[i] {
					return a[i] < b[i]
				}
			}
			return false
		}
	}
	return x < y
}
