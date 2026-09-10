package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// A namespace's worth of Secrets as the PXC operator leaves it: the cluster's own credentials,
// the TLS chain (PEM, which is text), and one key that is genuinely binary.
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

const secretListJSON = `{"apiVersion":"v1","kind":"List","items":[
  {"metadata":{"name":"k3d-00-secrets","namespace":"default","creationTimestamp":"2026-09-10T09:00:00Z",
     "labels":{"app.kubernetes.io/instance":"k3d-00"}},
   "type":"Opaque",
   "data":{"root":"` + "cm9vdF9wYXNzd29yZA==" + `","monitor":"` + "bW9uaXRvcg==" + `"}},
  {"metadata":{"name":"k3d-00-ssl","namespace":"default"},"type":"kubernetes.io/tls",
   "immutable":true,
   "data":{"tls.crt":"` + "LS0tLS1CRUdJTiBDRVJU" + `","tls.key":"` + "AAECf3//AA==" + `"}}
]}`

func TestParseK3DObjectListHidesValues(t *testing.T) {
	objs, err := parseK3DObjectList("secret", []byte(secretListJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("expected 2 secrets, got %d", len(objs))
	}
	if objs[0].Name != "k3d-00-secrets" || objs[1].Name != "k3d-00-ssl" {
		t.Errorf("not sorted by name: %s, %s", objs[0].Name, objs[1].Name)
	}
	// The list is the one thing that must never carry a password — it is what a panel renders
	// before anybody has asked for a specific object.
	for _, o := range objs {
		for _, e := range o.Entries {
			if e.Value != "" {
				t.Errorf("%s/%s: the list leaked a value", o.Name, e.Key)
			}
			if e.Size == 0 {
				t.Errorf("%s/%s: no size, so the list says nothing at all", o.Name, e.Key)
			}
		}
	}
	// Key names and sizes are the point of the list, and the size is of the DECODED value.
	first := objs[0]
	if len(first.Entries) != 2 || first.Entries[0].Key != "monitor" || first.Entries[1].Key != "root" {
		t.Fatalf("keys not listed and sorted: %+v", first.Entries)
	}
	if first.Entries[1].Size != len("root_password") {
		t.Errorf("size should be the decoded length, got %d", first.Entries[1].Size)
	}
	if !objs[1].Immutable {
		t.Error("an immutable secret must say so — it is why an edit will be refused")
	}
	if objs[1].Type != "kubernetes.io/tls" {
		t.Errorf("type = %q", objs[1].Type)
	}
}

func TestParseK3DObjectDecodesTextAndRefusesBinary(t *testing.T) {
	var doc struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(secretListJSON), &doc); err != nil {
		t.Fatal(err)
	}
	obj, err := parseK3DObject("secret", doc.Items[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := map[string]string{}
	for _, e := range obj.Entries {
		got[e.Key] = e.Value
	}
	if got["root"] != "root_password" {
		t.Errorf("root = %q, want the decoded password", got["root"])
	}

	// The TLS secret's key is bytes that are not text: reported with its size, never with a
	// value, because round-tripping it through a text box would corrupt it.
	tls, err := parseK3DObject("secret", doc.Items[1])
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]k3dObjEntry{}
	for _, e := range tls.Entries {
		byKey[e.Key] = e
	}
	if byKey["tls.crt"].Binary || byKey["tls.crt"].Value == "" {
		t.Errorf("a PEM certificate is text and should be editable: %+v", byKey["tls.crt"])
	}
	if !byKey["tls.key"].Binary || byKey["tls.key"].Value != "" {
		t.Errorf("a binary key must carry no value: %+v", byKey["tls.key"])
	}
}

func TestParseK3DConfigMapKeepsPlainTextAndBinaryDataApart(t *testing.T) {
	cm := []byte(`{"metadata":{"name":"k3d-00-pxc","namespace":"default"},
	  "data":{"my.cnf":"[mysqld]\nmax_connections=1000\n"},
	  "binaryData":{"blob":"` + b64("\x00\x01binary") + `"}}`)
	obj, err := parseK3DObject("configmap", cm)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(obj.Entries) != 2 {
		t.Fatalf("expected both keys, got %+v", obj.Entries)
	}
	byKey := map[string]k3dObjEntry{}
	for _, e := range obj.Entries {
		byKey[e.Key] = e
	}
	// A ConfigMap's data is NOT base64 — decoding it would produce nonsense.
	if !strings.Contains(byKey["my.cnf"].Value, "max_connections=1000") {
		t.Errorf("my.cnf = %q", byKey["my.cnf"].Value)
	}
	if !byKey["blob"].Binary || byKey["blob"].Value != "" {
		t.Errorf("binaryData is binary by definition: %+v", byKey["blob"])
	}
}

func TestK3DObjPatchEncodesPerKind(t *testing.T) {
	// A Secret's values are base64 on the way in; a ConfigMap's are not.
	p, err := k3dObjPatch("secret", map[string]string{"root": "new_password"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p), b64("new_password")) {
		t.Errorf("a secret's value must be base64: %s", p)
	}
	if strings.Contains(string(p), "new_password") {
		t.Errorf("the plain value must not travel in a secret patch: %s", p)
	}
	p, err = k3dObjPatch("configmap", map[string]string{"my.cnf": "[mysqld]\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p), `"[mysqld]\n"`) {
		t.Errorf("a configmap's value stays plain: %s", p)
	}

	// A removed key is a JSON null — that is what a merge patch means by "delete".
	p, err = k3dObjPatch("secret", nil, []string{"monitor"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p), `"monitor":null`) {
		t.Errorf("a removal should be null: %s", p)
	}

	// An empty edit and a key that is not a Kubernetes key are both refused before kubectl.
	if _, err := k3dObjPatch("secret", nil, nil); err == nil {
		t.Error("an empty patch must be refused")
	}
	for _, bad := range []string{"my key", "../etc/passwd", ""} {
		if _, err := k3dObjPatch("configmap", map[string]string{bad: "x"}, nil); err == nil {
			t.Errorf("key %q was accepted", bad)
		}
	}
}

func TestK3DObjTextual(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"root_password", true},
		{"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n", true},
		{"[mysqld]\nmax_connections=1000\n", true},
		{"café", true},
		{"\x00\x01\x02", false},
		{"has\x07a bell", false},
		{"\xff\xfe not utf8", false},
	}
	for _, c := range cases {
		if got := k3dObjTextual([]byte(c.in)); got != c.want {
			t.Errorf("k3dObjTextual(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCertManagerManifestURL(t *testing.T) {
	url := certManagerManifestURL(certManagerVersion)
	if !strings.HasPrefix(url, "https://github.com/cert-manager/cert-manager/releases/download/v") ||
		!strings.HasSuffix(url, "/cert-manager.yaml") {
		t.Errorf("not the release manifest: %s", url)
	}
	if !strings.Contains(url, certManagerVersion) {
		t.Errorf("the pinned version is not in the URL: %s", url)
	}
	// The webhook probe must be a Certificate — it is only there to make the API server call
	// cert-manager's validating webhook — and it must never be applied for real.
	if !strings.Contains(string(certManagerProbe), "kind: Certificate") {
		t.Error("the probe is not a Certificate")
	}
}

// The version the canvas prints on the checkbox and the version the deploy installs must be the
// same string. They live in two languages, and "cert-manager v1.21.1" on a form that installed
// something else is worse than saying nothing at all.
func TestCertManagerVersionMatchesTheCanvas(t *testing.T) {
	js, err := os.ReadFile("web/src/pages/StackDesigner.jsx")
	if err != nil {
		t.Skip("StackDesigner.jsx not readable")
	}
	want := "export const CERT_MANAGER_VERSION = '" + certManagerVersion + "'"
	if !strings.Contains(string(js), want) {
		t.Errorf("StackDesigner.jsx does not carry %s — expected the line %q", certManagerVersion, want)
	}
}
