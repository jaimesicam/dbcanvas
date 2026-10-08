package main

import "testing"

func TestDBUsersValidate(t *testing.T) {
	ok := dbUsersReq{Action: "create", Name: "app", Host: "%", Password: "s3cret-pass", Privilege: "readwrite", Database: "shop"}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []dbUsersReq{
		{Action: "create", Name: "app'; DROP", Host: "%", Password: "longenough", Privilege: "readonly"},
		{Action: "create", Name: "app", Host: "%", Password: "it's-quoted", Privilege: "readonly"},
		{Action: "create", Name: "app", Host: "%", Password: "short", Privilege: "readonly"},
		{Action: "create", Name: "app", Host: "%", Password: "longenough", Privilege: "god"},
		{Action: "create", Name: "app", Host: "a b", Password: "longenough", Privilege: "readonly"},
		{Action: "drop", Name: "mysql.sys", Host: "localhost"},
		{Action: "rename", Name: "app", Host: "%"},
	} {
		if bad.validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestMergeAccounts(t *testing.T) {
	tp := swTopo{Kind: "mysql", Members: []swMember{{Node: designNode{ID: "a"}}, {Node: designNode{ID: "b"}}, {Node: designNode{ID: "c"}}}}
	accs := []memberAccounts{
		{"app@%": {"h1", "SELECT"}, "mysql.sys@localhost": {"x", ""}},
		{"app@%": {"h1", "SELECT"}},
		{"app@%": {"h2", "SELECT"}, "old@%": {"h3", ""}},
	}
	us := mergeAccounts(tp, tp.Members[0], accs)
	if len(us) != 2 {
		t.Fatalf("users = %+v", us)
	}
	app := us[0]
	if app.Name != "app" || app.Host != "%" || len(app.On) != 3 || len(app.Differs) != 1 || app.Differs[0] != "c" {
		t.Fatalf("app = %+v", app)
	}
	if us[1].Name != "old" || len(us[1].On) != 1 {
		t.Fatalf("old = %+v", us[1])
	}
}
