package main

import "testing"

func k8sTestPod(name, phase string, labels map[string]string, containers ...string) k8sTargetPod {
	var p k8sTargetPod
	p.Metadata.Name, p.Metadata.Labels, p.Status.Phase = name, labels, phase
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, struct {
			Name string `json:"name"`
		}{c})
	}
	return p
}

// The sim's members are the pods that run the database: not proxies, routers,
// config servers or pods that are not running.
func TestPerfSimK8sDataPod(t *testing.T) {
	sts := func(name string, extra map[string]string) map[string]string {
		l := map[string]string{k8sPodNameSelector: name}
		for k, v := range extra {
			l[k] = v
		}
		return l
	}
	for _, c := range []struct {
		pod    k8sTargetPod
		engine string
		want   bool
		rs     string
	}{
		{k8sTestPod("c-pxc-0", "Running", sts("c-pxc-0", nil), "pxc", "logs"), dexMySQL, true, ""},
		{k8sTestPod("c-haproxy-0", "Running", sts("c-haproxy-0", nil), "haproxy", "pxc-monit"), dexMySQL, false, ""},
		{k8sTestPod("c-mysql-1", "Pending", sts("c-mysql-1", nil), "mysql"), dexMySQL, false, ""},
		{k8sTestPod("c-rs0-1", "Running", sts("c-rs0-1", map[string]string{"app.kubernetes.io/replset": "rs0", "app.kubernetes.io/component": "mongod"}), "mongod"), dexMongoDB, true, "rs0"},
		{k8sTestPod("c-cfg-0", "Running", sts("c-cfg-0", map[string]string{"app.kubernetes.io/replset": "cfg", "app.kubernetes.io/component": "cfg"}), "mongod"), dexMongoDB, false, ""},
		{k8sTestPod("c-mongos-0", "Running", sts("c-mongos-0", map[string]string{"app.kubernetes.io/component": "mongos"}), "mongos"), dexMongoDB, false, ""},
		{k8sTestPod("c-instance1-abcd-0", "Running", sts("c-instance1-abcd-0", nil), "database", "replication-cert-copy"), dexPostgres, true, ""},
		{k8sTestPod("c-pgbouncer-x", "Running", map[string]string{}, "pgbouncer"), dexPostgres, false, ""},
		{k8sTestPod("c-1", "Running", map[string]string{"cnpg.io/instanceName": "c-1"}, "postgres"), dexPostgres, true, ""},
	} {
		ok, rs := perfSimK8sDataPod(c.pod, c.engine)
		if ok != c.want || (ok && rs != c.rs) {
			t.Errorf("%s: %v %q, want %v %q", c.pod.Metadata.Name, ok, rs, c.want, c.rs)
		}
	}
	if sel := perfSimK8sSelector(k8sTestPod("c-1", "Running", map[string]string{"cnpg.io/instanceName": "c-1"})); sel["cnpg.io/instanceName"] != "c-1" || len(sel) != 1 {
		t.Errorf("cnpg selector %v", sel)
	}
	if sel := perfSimK8sSelector(k8sTestPod("c-pxc-2", "Running", map[string]string{k8sPodNameSelector: "c-pxc-2", "app": "x"})); sel[k8sPodNameSelector] != "c-pxc-2" || len(sel) != 1 {
		t.Errorf("statefulset selector %v", sel)
	}
}
