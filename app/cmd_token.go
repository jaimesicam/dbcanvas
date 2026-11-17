package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
)

// cmd_token.go — `dbcanvas token`: API tokens for a script, or for another user of this
// computer, who reach dbcanvas through its browser port with the token and nothing else
// (soloweb.go). The owner needs none: the control socket is theirs.

// labelFlags collects repeated --label k=v.
type labelFlags map[string]string

func (l labelFlags) String() string { return labelString(l) }
func (l labelFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("--label %q: want key=value", v)
	}
	l[strings.TrimSpace(k)] = strings.TrimSpace(val)
	return nil
}

func cliToken(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "create", "new":
		return cliTokenCreate(args)
	case "ls", "list", "":
		return withDBCanvas(cliTokenList)
	case "rm", "revoke", "delete":
		if len(args) != 1 {
			return usageErr("token rm")
		}
		return withDBCanvas(func(c ctl) error { return cliTokenRevoke(c, args[0]) })
	}
	return fmt.Errorf("unknown `token` command %q: use token create, token ls or token rm", sub)
}

func cliTokenCreate(args []string) error {
	fs := cmdFlags("token create")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	if len(rest) != 1 {
		return usageErr("token create")
	}
	scope, days := new(string), new(int)
	*scope, *days = flagString(fs, "scope"), flagInt(fs, "days")
	labels := labelFlags{}
	for _, l := range flagList(fs, "label") {
		if err := labels.Set(l); err != nil {
			return flagErr(err)
		}
	}
	limit := &tokenLimit{OwnStacks: flagBool(fs, "own-stacks"), MaxTTL: strings.TrimSpace(flagString(fs, "max-ttl"))}
	if len(labels) > 0 {
		limit.Labels = labels
	}
	for _, t := range strings.Split(flagString(fs, "deny"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			limit.Deny = append(limit.Deny, t)
		}
	}
	if err := limit.validate(); err != nil {
		return err
	}
	body := map[string]any{"name": rest[0], "scope": *scope, "days": *days}
	if !limit.empty() {
		body["limit"] = limit
	}
	return withDBCanvas(func(c ctl) error {
		var out struct {
			Secret string   `json:"secret"`
			Token  APIToken `json:"token"`
		}
		if err := c.do("POST", "/api/tokens", body, &out); err != nil {
			return err
		}
		var st struct {
			URL string `json:"url"`
		}
		c.do("GET", "/api/solo/status", nil, &st)
		fmt.Printf("Token %d, %q (%s scope)", out.Token.ID, out.Token.Name, out.Token.Scope)
		if out.Token.Limit != nil {
			fmt.Printf(", limited to %s", out.Token.Limit.describe())
		}
		if out.Token.ExpiresAt != nil {
			fmt.Printf(", expires %s", *out.Token.ExpiresAt)
		}
		fmt.Println(".")
		fmt.Println("Its secret is shown this once:")
		fmt.Println()
		fmt.Println("  " + out.Secret)
		fmt.Println()
		fmt.Printf("Use it as a bearer token on %s/, for example:\n", firstNonEmpty(st.URL, "http://127.0.0.1:"+soloPort))
		fmt.Printf("  curl -H 'Authorization: Bearer %s' %s/api/stacks\n", out.Secret, firstNonEmpty(st.URL, "http://127.0.0.1:"+soloPort))
		fmt.Println("While dbcanvas is stopped nothing answers it; it starts again with your next dbcanvas command.")
		return nil
	})
}

func cliTokenList(c ctl) error {
	var out struct {
		Tokens []APIToken `json:"tokens"`
	}
	if err := c.do("GET", "/api/tokens", nil, &out); err != nil {
		return err
	}
	if len(out.Tokens) == 0 {
		fmt.Println("No API tokens. `dbcanvas token create NAME` makes one.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSCOPE\tSTATE\tEXPIRES\tLAST USED\tLIMIT")
	for _, t := range out.Tokens {
		exp, used, lim := "never", "-", "-"
		if t.ExpiresAt != nil {
			exp = *t.ExpiresAt
		}
		if t.LastUsedAt != nil {
			used = *t.LastUsedAt
		}
		if t.Limit != nil {
			lim = t.Limit.describe()
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, t.Scope, t.State, exp, used, lim)
	}
	return tw.Flush()
}

func cliTokenRevoke(c ctl, ref string) error {
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return fmt.Errorf("token rm takes the token's ID, from dbcanvas token ls")
	}
	if err := c.do("DELETE", fmt.Sprintf("/api/tokens/%d", id), nil, nil); err != nil {
		return err
	}
	fmt.Printf("Revoked token %d.\n", id)
	return nil
}
