package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// cmd_network.go — `dbcanvas network check`: where the next stack network would go, and
// everything that rules a range out (subnets.go), without creating anything. It reads the
// Docker daemon and this host directly, so it needs no running dbcanvas: run it on a shared
// host before deploying there.

func cliNetwork(args []string) error {
	if len(args) != 1 || args[0] != "check" {
		return fmt.Errorf("usage: dbcanvas network check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	d := NewDocker(dockerSocket())
	if err := d.Ping(ctx); err != nil {
		return fmt.Errorf("Docker does not answer on %s: %v", dockerSocket(), err)
	}
	space, err := subnetSearchSpace()
	if err != nil {
		return err
	}
	named, err := d.namedNetworkSubnets(ctx)
	if err != nil {
		return err
	}
	pools, host, k8s := d.DefaultPools(ctx), hostSubnets(), mustCIDRs(k8sInternalRanges)

	from := "the default; set networkRange in ~/.dbcanvas/config.yaml to change it"
	if os.Getenv("DBCANVAS_NETWORK_RANGE") != "" {
		from = "networkRange in config.yaml, or DBCANVAS_NETWORK_RANGE"
	}
	fmt.Printf("Stack networks are /%d ranges found free in %s (%s).\n\n", stackPrefix, cidrList(space), from)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AVOIDED\tRANGE\tWHY")
	for _, n := range named {
		fmt.Fprintf(tw, "Docker network %s\t%s\tin use\n", n.Name, n.Net)
	}
	for _, n := range pools {
		fmt.Fprintf(tw, "Docker default pool\t%s\tDocker gives networks from it; one inside takes the whole pool from others\n", n)
	}
	seen := map[string]bool{}
	for _, n := range host {
		if !seen[n.String()] {
			seen[n.String()] = true
			fmt.Fprintf(tw, "this host\t%s\tan interface or route of this host\n", n)
		}
	}
	for _, n := range k8s {
		fmt.Fprintf(tw, "Kubernetes\t%s\tpods or services inside a K3D or Kubeadm cluster\n", n)
	}
	tw.Flush()

	var avoid []*net.IPNet
	for _, n := range named {
		avoid = append(avoid, n.Net)
	}
	avoid = append(append(append(avoid, pools...), host...), k8s...)
	fmt.Println()
	if next, ok := pickSubnet(space, avoid, instanceName); ok {
		fmt.Printf("The next stack network for instance %s would be %s.\n", instanceName, next)
	} else {
		fmt.Printf("No /%d is free in %s: remove unused Docker networks, or name more room in networkRange.\n", stackPrefix, cidrList(space))
	}
	fmt.Println(strings.TrimSpace(`
A network this host reaches only through its default gateway (a company LAN, a VPN) is not
listed above and cannot be: if it is inside the search range, set networkRange to a range
that is unused on your network.`))
	return nil
}
