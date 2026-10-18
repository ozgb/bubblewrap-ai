package main

import (
	"io"
	"slices"
	"strings"
	"testing"
)

func TestParseGhSafeRefuses(t *testing.T) {
	cases := map[string][]string{
		"no args":               {},
		"missing -R":            {"blocked-by", "63"},
		"-R after subcommand":   {"blocked-by", "63", "-R", "o/r"},
		"repo with extra path":  {"-R", "o/r/../../user", "blocked-by", "63"},
		"repo with query":       {"-R", "o/r?x=1", "blocked-by", "63"},
		"bare owner":            {"-R", "o", "blocked-by", "63"},
		"unknown subcommand":    {"-R", "o/r", "delete", "63"},
		"too few numbers":       {"-R", "o/r", "block", "63"},
		"too many numbers":      {"-R", "o/r", "blocked-by", "63", "64"},
		"trailing flag":         {"-R", "o/r", "blocked-by", "63", "--method=DELETE"},
		"non-numeric issue":     {"-R", "o/r", "blocked-by", "63/comments"},
		"zero issue":            {"-R", "o/r", "blocked-by", "0"},
		"leading zero":          {"-R", "o/r", "blocked-by", "063"},
		"negative issue":        {"-R", "o/r", "blocked-by", "-1"},
		"file reference":        {"-R", "o/r", "block", "63", "@/etc/passwd"},
		"empty issue":           {"-R", "o/r", "blocked-by", ""},
		"issue with whitespace": {"-R", "o/r", "blocked-by", "63 "},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := parseGhSafe(args); err == nil {
				t.Fatalf("parseGhSafe(%q) accepted", args)
			}
		})
	}
}

func TestGhSafeCalls(t *testing.T) {
	const repo = "ozgb/byte-tactics-moddable"
	cases := []struct {
		name string
		args []string
		want [][]string
	}{
		{
			name: "blocked-by lists",
			args: []string{"blocked-by", "63"},
			want: [][]string{{"api", "--paginate", "repos/" + repo + "/issues/63/dependencies/blocked_by", "--jq", `.[] | "#\(.number) \(.state) \(.title)"`}},
		},
		{
			name: "block resolves the blocker's id first",
			args: []string{"block", "63", "12"},
			want: [][]string{
				{"api", "repos/" + repo + "/issues/12", "--jq", ".id"},
				{"api", "--silent", "-X", "POST", "repos/" + repo + "/issues/63/dependencies/blocked_by", "-F", "issue_id=4242"},
			},
		},
		{
			name: "unblock deletes by the blocker's id",
			args: []string{"unblock", "63", "12"},
			want: [][]string{
				{"api", "repos/" + repo + "/issues/12", "--jq", ".id"},
				{"api", "--silent", "-X", "DELETE", "repos/" + repo + "/issues/63/dependencies/blocked_by/4242"},
			},
		},
		{
			name: "sub-issues lists",
			args: []string{"sub-issues", "63"},
			want: [][]string{{"api", "--paginate", "repos/" + repo + "/issues/63/sub_issues", "--jq", `.[] | "#\(.number) \(.state) \(.title)"`}},
		},
		{
			name: "add-sub resolves the child's id first",
			args: []string{"add-sub", "63", "70"},
			want: [][]string{
				{"api", "repos/" + repo + "/issues/70", "--jq", ".id"},
				{"api", "--silent", "-X", "POST", "repos/" + repo + "/issues/63/sub_issues", "-F", "sub_issue_id=4242"},
			},
		},
		{
			name: "remove-sub deletes by the child's id",
			args: []string{"remove-sub", "63", "70"},
			want: [][]string{
				{"api", "repos/" + repo + "/issues/70", "--jq", ".id"},
				{"api", "--silent", "-X", "DELETE", "repos/" + repo + "/issues/63/sub_issue", "-F", "sub_issue_id=4242"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got [][]string
			g := ghAPI{out: io.Discard, exec: func(stdout io.Writer, args ...string) error {
				got = append(got, args)
				if slices.Contains(args, ".id") {
					io.WriteString(stdout, "4242\n")
				}
				return nil
			}}
			if code := ghSafe(append([]string{"-R", repo}, tc.args...), g); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if !slices.EqualFunc(got, tc.want, slices.Equal) {
				t.Fatalf("gh calls:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestGhSafeRefusesBadID(t *testing.T) {
	var writes int
	g := ghAPI{out: io.Discard, exec: func(stdout io.Writer, args ...string) error {
		if slices.Contains(args, ".id") {
			io.WriteString(stdout, "null\n")
			return nil
		}
		writes++
		return nil
	}}
	if code := ghSafe([]string{"-R", "o/r", "block", "63", "12"}, g); code == 0 {
		t.Fatal("block with an unresolvable id succeeded")
	}
	if writes != 0 {
		t.Fatalf("%d write(s) sent after a failed id lookup", writes)
	}
}

func TestGhSafeUsageNamesEveryOp(t *testing.T) {
	for name := range ghSafeOps {
		if !strings.Contains(ghSafeUsage, " "+name+" ") {
			t.Errorf("usage does not mention %q", name)
		}
	}
}
