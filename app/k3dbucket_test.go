package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestBucketPodManifestCarriesTheCredentialsAndEndpoint(t *testing.T) {
	got := k3dBucketPodManifest("cluster1-dbcanvas-s3", "percona/percona-xtrabackup:8.0",
		"http://seaweedfs.stack:8333", "dbcanvas", "us-east-1", "cluster1-backup-s3", "")
	for _, want := range []string{
		"name: cluster1-dbcanvas-s3",
		"image: percona/percona-xtrabackup:8.0",
		`command: ["sleep", "infinity"]`,
		"restartPolicy: Never",
		`value: "http://seaweedfs.stack:8333"`,
		"name: cluster1-backup-s3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the toolbox manifest is missing %q:\n%s", want, got)
		}
	}
	// envFrom, not a mounted file: the Secret's keys are already named the way the AWS CLI reads
	// them, which is the whole reason no credentials are ever written into a command.
	if !strings.Contains(got, "envFrom:\n    - secretRef:") {
		t.Errorf("credentials must arrive as envFrom:\n%s", got)
	}
	// Without this the CLI spends a minute probing the EC2 metadata endpoint on every call.
	if !strings.Contains(got, "AWS_EC2_METADATA_DISABLED") {
		t.Errorf("the metadata probe must be disabled:\n%s", got)
	}
	// A `resources:` block would not be admitted on a small k3d budget — the same reason
	// k3dcr.go comments cr.yaml's out.
	if strings.Contains(got, "resources:") {
		t.Errorf("the toolbox must not request resources:\n%s", got)
	}
}

// An endpoint or bucket that ended the YAML string early would produce a manifest that applies as
// something other than what was meant. %q is what prevents it, and this is the test that says so.
func TestBucketPodManifestQuotesItsValues(t *testing.T) {
	got := k3dBucketPodManifest("p", "img", `http://x"#evil`, `b"ucket`, "r", "s", "")
	if strings.Contains(got, `value: "http://x"#evil"`) {
		t.Errorf("the endpoint escaped its quoting:\n%s", got)
	}
	if !strings.Contains(got, `value: "http://x\"#evil"`) {
		t.Errorf("the endpoint should be escaped:\n%s", got)
	}
	if !strings.Contains(got, `value: "b\"ucket"`) {
		t.Errorf("the bucket should be escaped:\n%s", got)
	}
}

func TestBucketPodNameIsPerCluster(t *testing.T) {
	if got := k3dBucketPodName("cluster1"); got != "cluster1-dbcanvas-s3" {
		t.Errorf("got %q", got)
	}
	if k3dBucketPodName("a") == k3dBucketPodName("b") {
		t.Error("two clusters must not share a toolbox — their credentials may differ")
	}
}

func TestCleanBucketKeyRefusesTraversalAndControlCharacters(t *testing.T) {
	for _, bad := range []string{
		"../etc/passwd",
		"a/../../b",
		"a/./b",
		"a\nb",
		"a\x00b",
		strings.Repeat("a", k3dBucketKeyMax+1),
	} {
		if _, err := cleanK3DBucketKey(bad); err == nil {
			t.Errorf("%q should have been refused", bad)
		}
	}
	// S3 keys really do contain spaces, '+', '=' and parentheses, and a backup tool's prefixes
	// contain dots and dashes. Refusing those would make the browser useless on real buckets.
	for _, good := range []string{
		"", "/", "pbm/cluster1", "pgbackrest/cluster1/repo1/backup/db",
		"2026-09-11-12:00:00F/backup.xbstream.gz", "a b (1)+c=d", "xtrabackup_info",
	} {
		if _, err := cleanK3DBucketKey(good); err != nil {
			t.Errorf("%q should have been accepted: %v", good, err)
		}
	}
	// The leading slash goes, so "/a" and "a" name the same object.
	if got, _ := cleanK3DBucketKey("/pbm/cluster1"); got != "pbm/cluster1" {
		t.Errorf("the leading slash should be stripped, got %q", got)
	}
}

func TestBucketListArgsFoldAtTheSlash(t *testing.T) {
	args := k3dBucketListArgs("dbcanvas", "pbm/cluster1", "")
	joined := strings.Join(args.Cmd, " ")
	for _, want := range []string{
		"aws s3api list-objects-v2", "--bucket dbcanvas", "--delimiter /", "--prefix pbm/cluster1/",
		"--output json",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the listing call is missing %q: %v", want, args.Cmd)
		}
	}
	// s3api, not `aws s3 ls`: the high-level command prints a text table with no continuation
	// token, and paging is the only way a pgBackRest repository is navigable at all.
	if strings.Contains(joined, " s3 ls") {
		t.Errorf("the listing must use s3api: %v", args.Cmd)
	}
	// The root listing has no --prefix at all; "--prefix /" would match nothing.
	if strings.Contains(strings.Join(k3dBucketListArgs("b", "", "").Cmd, " "), "--prefix") {
		t.Error("a root listing should not carry a prefix")
	}
	if !strings.Contains(strings.Join(k3dBucketListArgs("b", "", "tok").Cmd, " "), "--starting-token tok") {
		t.Error("the continuation token should be passed through")
	}
}

func TestParseBucketListSeparatesFoldersFromObjects(t *testing.T) {
	objs, next, err := parseK3DBucketList([]byte(`{
      "CommonPrefixes":[{"Prefix":"pbm/cluster1/2026/"}],
      "Contents":[
        {"Key":"pbm/cluster1/","Size":0,"LastModified":"2026-09-11T12:00:00Z"},
        {"Key":"pbm/cluster1/backup.md5","Size":33,"LastModified":"2026-09-11T12:05:00Z"},
        {"Key":"pbm/cluster1/dump.gz","Size":10485760,"LastModified":"2026-09-11T12:06:00Z"}],
      "IsTruncated":false}`), "pbm/cluster1/")
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Errorf("an untruncated listing has no continuation token, got %q", next)
	}
	if len(objs) != 3 {
		t.Fatalf("got %d entries, want 3 (the folder marker for the prefix itself is dropped): %+v", len(objs), objs)
	}
	// Folders first, so descending into one is at the top of the list where a file browser puts it.
	if !objs[0].Dir || objs[0].Name != "2026" || objs[0].Key != "pbm/cluster1/2026" {
		t.Errorf("the common prefix should be the first row: %+v", objs[0])
	}
	if objs[1].Dir || objs[1].Name != "backup.md5" || objs[1].Size != 33 {
		t.Errorf("objects should carry their own name and size: %+v", objs[1])
	}
	if objs[2].Modified != "2026-09-11T12:06:00Z" {
		t.Errorf("the modified time should survive: %+v", objs[2])
	}
	// The zero-length key equal to the prefix is the folder marker for the directory being
	// listed; showing it would put every folder inside itself.
	for _, o := range objs {
		if o.Key == "pbm/cluster1/" {
			t.Error("the prefix's own folder marker should not be listed")
		}
	}
}

func TestParseBucketListPagesWhenTruncated(t *testing.T) {
	_, next, err := parseK3DBucketList([]byte(
		`{"Contents":[{"Key":"a","Size":1}],"IsTruncated":true,"NextContinuationToken":"tok"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "tok" {
		t.Errorf("a truncated listing should hand back its token, got %q", next)
	}
}

// An empty bucket answers with an empty body on some S3 implementations rather than `{}`, and an
// empty bucket must read as empty rather than as an error.
func TestParseBucketListHandlesAnEmptyAnswer(t *testing.T) {
	objs, next, err := parseK3DBucketList([]byte("  \n"), "")
	if err != nil || len(objs) != 0 || next != "" {
		t.Errorf("an empty answer should be an empty listing: %v %v %v", objs, next, err)
	}
	if _, _, err := parseK3DBucketList([]byte("not json"), ""); err == nil {
		t.Error("a non-JSON answer should be an error, not an empty listing")
	}
}

func TestBucketDeleteMessageDistinguishesNothingFromDone(t *testing.T) {
	if got := k3dBucketDeleteMessage(0, false); !strings.Contains(got, "nothing matched") {
		t.Errorf("deleting nothing must not read as success: %q", got)
	}
	if got := k3dBucketDeleteMessage(3, true); !strings.Contains(got, "would be") {
		t.Errorf("a dry run must not read as done: %q", got)
	}
	if got := k3dBucketDeleteMessage(1, false); got != "1 object deleted" {
		t.Errorf("got %q", got)
	}
	if got := k3dBucketDeleteMessage(3, false); got != "3 objects deleted" {
		t.Errorf("got %q", got)
	}
}

// The copyable command line is the one place quoting matters, because a person pastes it into a
// shell. A key with a space or a semicolon in it must come back runnable.
func TestShellQuotingOfTheCopyableCommand(t *testing.T) {
	got := shCommand([]string{"aws", "s3", "rm", "s3://b/a file; rm -rf /"})
	if got != `aws s3 rm 's3://b/a file; rm -rf /'` {
		t.Errorf("got %q", got)
	}
	if shCommand([]string{"aws", "s3", "ls"}) != "aws s3 ls" {
		t.Error("plain words should not be quoted")
	}
	if got := shQuote("it's"); got != `'it'\''s'` {
		t.Errorf("a single quote inside should be escaped, got %q", got)
	}
	if got := shQuote(""); got != "''" {
		t.Errorf("an empty word needs quotes to survive, got %q", got)
	}
}

func TestByteSizeLabel(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{{512, "512 B"}, {2048, "2.0 KiB"}, {5 << 20, "5.0 MiB"}, {3 << 30, "3.0 GiB"}} {
		if got := byteSizeLabel(c.n); got != c.want {
			t.Errorf("byteSizeLabel(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// A plain-HTTP store mounts nothing; an https one mounts the bundle and names it, because the
// AWS CLI is botocore and botocore does not read the system trust store (see seaweedTLSBundle).
func TestBucketPodManifestMountsTheStoreCertificateOnlyWhenThereIsOne(t *testing.T) {
	plain := k3dBucketPodManifest("p", "img", "http://sw:8333", "b", "r", "s", "")
	for _, unwanted := range []string{"AWS_CA_BUNDLE", "volumeMounts:", "volumes:"} {
		if strings.Contains(plain, unwanted) {
			t.Errorf("a plain-HTTP toolbox should not carry %q:\n%s", unwanted, plain)
		}
	}
	tls := k3dBucketPodManifest("p", "img", "https://sw:8333", "b", "r", "s", "c1-dbcanvas-s3-ca")
	for _, want := range []string{
		"name: AWS_CA_BUNDLE",
		"value: " + k3dBucketCAPath,
		"mountPath: /etc/dbcanvas-s3",
		"secretName: c1-dbcanvas-s3-ca",
	} {
		if !strings.Contains(tls, want) {
			t.Errorf("the TLS toolbox manifest is missing %q:\n%s", want, tls)
		}
	}
}

// The certificate is base64 in `data`, so a multi-line PEM cannot be broken by indentation.
func TestBucketCASecretCarriesThePEM(t *testing.T) {
	pem := []byte("-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n")
	got := k3dBucketCASecret("c1-dbcanvas-s3-ca", pem)
	if !strings.Contains(got, "name: c1-dbcanvas-s3-ca") || !strings.Contains(got, "ca.pem: ") {
		t.Errorf("the CA Secret is not shaped right:\n%s", got)
	}
	if strings.Contains(got, "BEGIN CERTIFICATE") {
		t.Errorf("the PEM should be base64, not inline:\n%s", got)
	}
	enc := base64.StdEncoding.EncodeToString(pem)
	if !strings.Contains(got, enc) {
		t.Errorf("the encoded PEM is missing:\n%s", got)
	}
}
