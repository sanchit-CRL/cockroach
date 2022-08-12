// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package issues

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/datadriven"
	"github.com/google/go-github/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPost(t *testing.T) {
	const (
		assignee    = "hodor" // fake GitHub handle we're returning as assignee
		milestone   = 2       // fake milestone we're using here
		issueID     = 1337    // issue ID returned in select test cases
		issueNumber = 30      // issue # returned in select test cases
	)

	opts := Options{
		Token:       "intentionally-unset",
		Org:         "cockroachdb",
		Repo:        "cockroach",
		SHA:         "abcd123",
		BuildTypeID: "nightly123",
		BuildID:     "8008135",
		ServerURL:   "https://teamcity.example.com",
		Branch:      "release-0.1",
		Tags:        "deadlock",
		Goflags:     "race",
	}

	type testCase struct {
		name                 string
		packageName          string
		testName             string
		message              string
		artifacts            string
		reproCmd             string
		reproTitle, reproURL string
	}

	testCases := []testCase{
		{
			name:        "failure",
			packageName: "github.com/cockroachdb/cockroach/pkg/storage",
			testName:    "TestReplicateQueueRebalance",
			message:     "	<autogenerated>:12: storage/replicate_queue_test.go:103, condition failed to evaluate within 45s: not balanced: [10 1 10 1 8]",
			reproCmd:    "make stressrace TESTS=TestReplicateQueueRebalance PKG=./pkg/storage TESTTIMEOUT=5m STRESSFLAGS='-timeout 5m' 2>&1",
		},
		{
			name:        "fatal",
			packageName: "github.com/cockroachdb/cockroach/pkg/storage",
			testName:    "TestGossipHandlesReplacedNode",
			message: `logging something
F170517 07:33:43.763059 69575 storage/replica.go:1360  [n3,s3,r1/3:/M{in-ax}] something bad happened:
foo
bar

goroutine 12 [running]:
  doing something

goroutine 13:
  hidden

`,
			reproCmd: "make stressrace TESTS=TestGossipHandlesReplacedNode PKG=./pkg/storage TESTTIMEOUT=5m STRESSFLAGS='-timeout 5m' 2>&1",
		},
		{
			name:        "panic",
			packageName: "github.com/cockroachdb/cockroach/pkg/storage",
			testName:    "TestGossipHandlesReplacedNode",
			message: `logging something
panic: something bad happened:

foo
bar

goroutine 12 [running]:
  doing something

goroutine 13:
  hidden

`,
			reproCmd: "make stressrace TESTS=TestGossipHandlesReplacedNode PKG=./pkg/storage TESTTIMEOUT=5m STRESSFLAGS='-timeout 5m' 2>&1",
		},
		{
			name:        "with-artifacts",
			packageName: "github.com/cockroachdb/cockroach/pkg/storage",
			testName:    "kv/splits/nodes=3/quiesce=true",
			message:     "The test failed on branch=master, cloud=gce:",
			artifacts:   "/kv/splits/nodes=3/quiesce=true",
			reproCmd:    "",
		},
		{
			name:        "rsg-crash",
			packageName: "github.com/cockroachdb/cockroach/pkg/sql/tests",
			testName:    "TestRandomSyntaxSQLSmith",
			message: `logging something
    rsg_test.go:755: Crash detected: server panic: pq: internal error: something bad
		SELECT
			foo
		FROM
			bar
		LIMIT
			33:::INT8;
        
        Stack trace:
    rsg_test.go:764: 266003 executions, 235459 successful
    rsg_test.go:575: To reproduce, use schema:
    rsg_test.go:577: 
        	CREATE TABLE table1 (col1_0 BOOL);
        ;
    rsg_test.go:577: 
        
        CREATE TYPE greeting AS ENUM ('hello', 'howdy', 'hi', 'good day', 'morning');
        ;
    rsg_test.go:579: 
    rsg_test.go:580: -- test log scope end --
test logs left over in: /go/src/github.com/cockroachdb/cockroach/artifacts/logTestRandomSyntaxSQLSmith460792454
--- FAIL: TestRandomSyntaxSQLSmith (300.69s)
`,
			reproCmd: "make test TESTS=TestRandomSyntaxSQLSmith PKG=./pkg/sql/tests 2>&1",
		},
		{
			name:        "failure-with-url",
			packageName: "github.com/cockroachdb/cockroach/pkg/cmd/roachtest",
			testName:    "some-roachtest",
			message:     "boom",
			reproURL:    "https://github.com/cockroachdb/cockroach",
			reproTitle:  "FooBar README",
		},
	}

	testByName := func(t *testing.T, name string) testCase {
		for _, tc := range testCases {
			if tc.name == name {
				return tc
			}
		}
		t.Fatalf("test case %s not found", name)
		return testCase{} // unreachable
	}

	const (
		foundNoIssue                 = "no-issue"
		foundOnlyMatchingIssue       = "matching-issue"
		foundMatchingAndRelatedIssue = "matching-and-related-issue"
		foundOnlyRelatedIssue        = "related-issue"
	)

	matchingIssue := github.Issue{
		Title:  github.String("boom"),
		Number: github.Int(issueNumber),
		Labels: []github.Label{{
			Name: github.String("C-test-failure"),
			URL:  github.String("fake"),
		}, {
			Name: github.String("O-robot"),
			URL:  github.String("fake"),
		}, {
			Name: github.String("release-0.1"),
			URL:  github.String("fake"),
		}},
	}
	relatedIssue := github.Issue{
		Title:  github.String("boom related"),
		Number: github.Int(issueNumber + 1),
		Labels: []github.Label{{
			Name: github.String("C-test-failure"),
			URL:  github.String("fake"),
		}, {
			Name: github.String("O-robot"),
			URL:  github.String("fake"),
		}, {
			Name: github.String("release-0.2"), // here's the mismatch
			URL:  github.String("fake"),
		}},
	}

	// This test determines from the file name what logic to run. The first
	// subgroup determines the test case (from the above slice). The second
	// determines whether matching/related issues exist.
	foundIssueScenarios := map[string][][]github.Issue{
		foundNoIssue:                 {{}, {}},
		foundOnlyMatchingIssue:       {{matchingIssue}, {}},
		foundMatchingAndRelatedIssue: {{matchingIssue}, {relatedIssue}},
		foundOnlyRelatedIssue:        {{}, {relatedIssue}},
	}
	var sKeys []string
	for k := range foundIssueScenarios {
		sKeys = append(sKeys, k)
	}
	re := regexp.MustCompile(`^(.+?)-(` + strings.Join(sKeys, "|") + `)\.txt$`)
	datadriven.Walk(t, testutils.TestDataPath(t, "post"), func(t *testing.T, path string) {
		datadriven.RunTest(t, path, func(t *testing.T, d *datadriven.TestData) string {
			basename := filepath.Base(path)
			sl := re.FindStringSubmatch(basename)
			require.Len(t, sl, 3, "%s couldn't be interpreted as a test case", basename)
			name, foundIssue := sl[1], sl[2]
			c := testByName(t, name)
			results, ok := foundIssueScenarios[foundIssue]
			require.True(t, ok, "missing issue scenario %s", foundIssue)

			var buf strings.Builder
			opts := opts // play it safe since we're mutating it below
			opts.getLatestTag = func() (string, error) {
				const tag = "v3.3.0"
				_, _ = fmt.Fprintf(&buf, "getLatestTag: result %s\n", tag)
				return tag, nil
			}

			p := &poster{
				Options: &opts,
			}

			createdIssue := false
			p.createIssue = func(_ context.Context, owner string, repo string,
				issue *github.IssueRequest) (*github.Issue, *github.Response, error) {
				createdIssue = true
				body := *issue.Body
				issue.Body = nil
				title := *issue.Title
				issue.Title = nil

				render := ghURL(t, title, body)
				t.Log(render)
				_, _ = fmt.Fprintf(&buf, "createIssue owner=%s repo=%s:\n%s\n\n%s\n\n%s\n\nRendered: %s", owner, repo, github.Stringify(issue), title, body, render)
				return &github.Issue{ID: github.Int64(issueID)}, nil, nil
			}

			p.searchIssues = func(_ context.Context, query string,
				opt *github.SearchOptions) (*github.IssuesSearchResult, *github.Response, error) {
				result := &github.IssuesSearchResult{}

				require.NotEmpty(t, results)
				result.Issues, results = results[0], results[1:]

				result.Total = github.Int(len(result.Issues))
				_, _ = fmt.Fprintf(&buf, "searchIssue %s: %s\n", query, github.Stringify(&result.Issues))
				return result, nil, nil
			}

			createdComment := false
			p.createComment = func(
				_ context.Context, owner string, repo string, number int, comment *github.IssueComment,
			) (*github.IssueComment, *github.Response, error) {
				assert.Equal(t, *matchingIssue.Number, number)
				createdComment = true
				render := ghURL(t, "<comment>", *comment.Body)
				t.Log(render)
				_, _ = fmt.Fprintf(&buf, "createComment owner=%s repo=%s issue=%d:\n\n%s\n\nRendered: %s", owner, repo, number, *comment.Body, render)
				return &github.IssueComment{}, nil, nil
			}

			p.listCommits = func(
				_ context.Context, owner string, repo string, opts *github.CommitsListOptions,
			) ([]*github.RepositoryCommit, *github.Response, error) {
				_, _ = fmt.Fprintf(&buf, "listCommits owner=%s repo=%s %s\n", owner, repo, github.Stringify(opts))
				assignee := assignee
				return []*github.RepositoryCommit{
					{
						Author: &github.User{
							Login: &assignee,
						},
					},
				}, nil, nil
			}

			p.listMilestones = func(_ context.Context, owner, repo string,
				_ *github.MilestoneListOptions) ([]*github.Milestone, *github.Response, error) {
				result := []*github.Milestone{
					{Title: github.String("3.3"), Number: github.Int(milestone)},
					{Title: github.String("3.2"), Number: github.Int(1)},
				}
				_, _ = fmt.Fprintf(&buf, "listMilestones owner=%s repo=%s: result %s\n", owner, repo, github.Stringify(result))
				return result, nil, nil
			}

			repro := UnitTestHelpCommand(c.reproCmd)
			if c.reproTitle != "" {
				repro = HelpCommandAsLink(c.reproTitle, c.reproURL)
			}
			req := PostRequest{
				PackageName:     c.packageName,
				TestName:        c.testName,
				Message:         c.message,
				Artifacts:       c.artifacts,
				MentionOnCreate: []string{"@cockroachdb/idonotexistbecausethisisatest"},
				HelpCommand:     repro,
				ExtraLabels:     []string{"release-blocker"},
				ExtraParams:     map[string]string{"ROACHTEST_cloud": "gce"},
			}
			require.NoError(t, p.post(context.Background(), UnitTestFormatter, req))

			switch foundIssue {
			case foundNoIssue, foundOnlyRelatedIssue:
				require.True(t, createdIssue)
				require.False(t, createdComment)
			case foundOnlyMatchingIssue, foundMatchingAndRelatedIssue:
				require.False(t, createdIssue)
				require.True(t, createdComment)
			default:
				t.Errorf("unhandled: %s", foundIssue)
			}

			return buf.String()
		})
	})
}

func TestPostEndToEnd(t *testing.T) {
	skip.IgnoreLint(t, "only for manual testing")

	env := map[string]string{
		// Adjust to your taste. Your token must have access and you must have a fork
		// of the cockroachdb/cockroach repo. Make sure you don't publicize the token
		// by pushing a branch.
		"GITHUB_ORG":       "tbg",
		"GITHUB_API_TOKEN": "",

		// These can be left untouched for a basic test.
		"GITHUB_REPO":      "cockroach",
		"BUILD_VCS_NUMBER": "deadbeef",
		"TC_SERVER_URL":    "https://teamcity.cockroachdb.com",
		"TC_BUILD_ID":      "12345",
		"TAGS":             "-endtoendenv",
		"GOFLAGS":          "-somegoflags",
		"TC_BUILD_BRANCH":  "release-19.2",
	}
	unset := setEnv(env)
	defer unset()

	params := map[string]string{
		"GOFLAGS":         "-race_test",
		"ROACHTEST_cloud": "test",
		"ROACHTEST_cpu":   "2",
	}

	req := PostRequest{
		PackageName: "github.com/cockroachdb/cockroach/pkg/foo/bar",
		TestName:    "TestFooBarBaz",
		Message:     "I'm a message",
		ExtraLabels: []string{"release-blocker"},
		ExtraParams: params,
		HelpCommand: UnitTestHelpCommand(""),
	}

	require.NoError(t, Post(context.Background(), UnitTestFormatter, req))
}

// setEnv overrides the env variables corresponding to the input map. The
// returned closure restores the status quo.
func setEnv(kv map[string]string) func() {
	undo := map[string]*string{}
	for key, value := range kv {
		val, ok := os.LookupEnv(key)
		if ok {
			undo[key] = &val
		} else {
			undo[key] = nil
		}

		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	return func() {
		for key, value := range undo {
			if value != nil {
				if err := os.Setenv(key, *value); err != nil {
					panic(err)
				}
			} else {
				if err := os.Unsetenv(key); err != nil {
					panic(err)
				}
			}
		}
	}
}

func ghURL(t *testing.T, title, body string) string {
	u, err := url.Parse("https://github.com/cockroachdb/cockroach/issues/new")
	require.NoError(t, err)
	q := u.Query()
	q.Add("title", title)
	q.Add("body", body)
	u.RawQuery = q.Encode()
	return u.String()
}
