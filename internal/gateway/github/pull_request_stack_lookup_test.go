package github

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGateway_PullRequestsForStackUpdate(t *testing.T) {
	requestNumber := 0
	gateway := newTestGateway(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requestNumber++
		var request struct {
			Query     string          `json:"query"`
			Variables json.RawMessage `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))

		switch requestNumber {
		case 1:
			assert.Contains(t, request.Query, "pr0:pullRequest(number: $pr0)")
			assert.Contains(t, request.Query, "pr3:pullRequest(number: $pr3)")
			assert.Contains(t, request.Query, "id,state,headRefName,baseRefName,headRepository{owner{login},name},stack{id}")
			assert.NotContains(t, request.Query, "labels")
			assert.NotContains(t, request.Query, "entries")
			assert.JSONEq(t, `{
				"owner": "acme",
				"repo": "repo",
				"pr0": 1,
				"pr1": 2,
				"pr2": 3,
				"pr3": 4
			}`, string(request.Variables))
			return graphQLResponse(`{
				"data": {"repository": {
					"pr0": {
						"id": "PR_1",
						"state": "OPEN",
						"headRefName": "bottom",
						"baseRefName": "main",
						"headRepository": {
							"owner": {"login": "acme"},
							"name": "repo"
						},
						"stack": {"id": "STACK_42"}
					},
					"pr1": {
						"id": "PR_2",
						"state": "OPEN",
						"headRefName": "top",
						"baseRefName": "bottom",
						"headRepository": {
							"owner": {"login": "acme"},
							"name": "repo"
						},
						"stack": {"id": "STACK_42"}
					},
					"pr2": {
						"state": "CLOSED",
						"headRepository": {
							"owner": {"login": "someone"},
							"name": "fork"
						},
						"stack": null
					},
					"pr3": null
				}}
			}`), nil

		case 2:
			assert.Equal(t,
				"query($ids:[ID!]!){nodes(ids: $ids){... on PullRequestStack{id,number,entries(first: 100){nodes{pullRequest{number,state,mergeQueueEntry{id},autoMergeRequest{enabledAt}}},pageInfo{endCursor,hasNextPage}}}}}",
				request.Query,
			)
			assert.JSONEq(t, `{"ids":["STACK_42"]}`, string(request.Variables))
			return graphQLResponse(`{
				"data": {"nodes": [{
					"id": "STACK_42",
					"number": 42,
					"entries": {"nodes": [
						{"pullRequest": {"number": 1, "state": "OPEN", "mergeQueueEntry": null, "autoMergeRequest": null}},
						{"pullRequest": {"number": 2, "state": "OPEN", "mergeQueueEntry": {"id":"QUEUE"}, "autoMergeRequest": null}},
						{"pullRequest": {"number": 5, "state": "MERGED"}}
					], "pageInfo": {"hasNextPage": false}}
				}]}
			}`), nil

		default:
			t.Fatalf("unexpected request %d", requestNumber)
			return nil, nil
		}
	}))

	got, err := gateway.PullRequestsForStackUpdate(
		t.Context(),
		"acme",
		"repo",
		[]int{1, 2, 3, 4},
	)
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal(t, ID("PR_1"), got[0].ID)
	assert.Equal(t, PullRequestStateOpen, got[0].State)
	assert.Equal(t, "bottom", got[0].HeadRefName)
	assert.Equal(t, "main", got[0].BaseRefName)
	assert.Equal(t, "acme", got[0].HeadRepositoryOwner)
	assert.Equal(t, "repo", got[0].HeadRepositoryName)
	require.NotNil(t, got[0].Stack)
	assert.Equal(t, 42, got[0].Stack.Number)
	assert.Equal(t, []PullRequestStackMember{
		{Number: 1, State: PullRequestStateOpen},
		{Number: 2, State: PullRequestStateOpen, Locked: true},
		{Number: 5, State: PullRequestStateMerged},
	}, got[0].Stack.Members)
	require.NotNil(t, got[1].Stack)
	assert.Equal(t, 42, got[1].Stack.Number)
	assert.Same(t, got[0].Stack, got[1].Stack)
	assert.Equal(t, PullRequestStateClosed, got[2].State)
	assert.Equal(t, "someone", got[2].HeadRepositoryOwner)
	assert.Equal(t, "fork", got[2].HeadRepositoryName)
	assert.Nil(t, got[2].Stack)
	assert.Nil(t, got[3])
	assert.Equal(t, 2, requestNumber)
}

func TestGateway_PullRequestsForStackUpdate_paginatesStackEntries(t *testing.T) {
	requestNumber := 0
	gateway := newTestGateway(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requestNumber++
		var request struct {
			Query     string          `json:"query"`
			Variables json.RawMessage `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))

		switch requestNumber {
		case 1:
			return graphQLResponse(`{
				"data": {"repository": {"pr0": {
					"state": "OPEN",
					"headRepository": {
						"owner": {"login": "acme"},
						"name": "repo"
					},
					"stack": {"id": "STACK_42"}
				}}}
			}`), nil
		case 2:
			assert.Contains(t, request.Query, "entries(first: 100)")
			assert.Contains(t, request.Query, "pageInfo{endCursor,hasNextPage}")
			return graphQLResponse(`{
				"data": {"nodes": [{
					"id": "STACK_42",
					"number": 42,
					"entries": {
						"nodes": [
							{"pullRequest": {"number": 1, "state": "OPEN"}},
							{"pullRequest": {"number": 2, "state": "MERGED"}}
						],
						"pageInfo": {"endCursor": "cursor-1", "hasNextPage": true}
					}
				}]}
			}`), nil
		case 3:
			assert.Equal(t,
				"query($after:String!$id:ID!){node(id: $id){... on PullRequestStack{entries(first: 100, after: $after){nodes{pullRequest{number,state,mergeQueueEntry{id},autoMergeRequest{enabledAt}}},pageInfo{endCursor,hasNextPage}}}}}",
				request.Query,
			)
			assert.JSONEq(t,
				`{"after":"cursor-1","id":"STACK_42"}`,
				string(request.Variables),
			)
			return graphQLResponse(`{
				"data": {"node": {"entries": {
					"nodes": [
						{"pullRequest": {"number": 3, "state": "OPEN", "autoMergeRequest": {"enabledAt": "2026-08-23T00:00:00Z"}}}
					],
					"pageInfo": {"hasNextPage": false}
				}}}
			}`), nil
		default:
			t.Fatalf("unexpected request %d", requestNumber)
			return nil, nil
		}
	}))

	got, err := gateway.PullRequestsForStackUpdate(
		t.Context(),
		"acme",
		"repo",
		[]int{1},
	)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].Stack)
	assert.Equal(t, []PullRequestStackMember{
		{Number: 1, State: PullRequestStateOpen},
		{Number: 2, State: PullRequestStateMerged},
		{Number: 3, State: PullRequestStateOpen, Locked: true},
	}, got[0].Stack.Members)
	assert.Equal(t, 3, requestNumber)
}

func TestGateway_PullRequestsForStackUpdate_empty(t *testing.T) {
	gateway := newTestGateway(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected request")
		return nil, nil
	}))

	got, err := gateway.PullRequestsForStackUpdate(t.Context(), "acme", "repo", nil)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestGateway_PullRequestsForStackUpdate_stackStopsResolving(t *testing.T) {
	requestNumber := 0
	gateway := newTestGateway(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requestNumber++
		switch requestNumber {
		case 1:
			return graphQLResponse(`{
				"data": {"repository": {"pr0": {
					"state": "OPEN",
					"headRepository": {
						"owner": {"login": "acme"},
						"name": "repo"
					},
					"stack": {"id": "STACK_42"}
				}}}
			}`), nil
		case 2:
			return graphQLResponse(`{"data":{"nodes":[null]}}`), nil
		default:
			t.Fatalf("unexpected request %d", requestNumber)
			return nil, nil
		}
	}))

	got, err := gateway.PullRequestsForStackUpdate(
		t.Context(),
		"acme",
		"repo",
		[]int{1},
	)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0])
	assert.Nil(t, got[0].Stack)
	assert.Equal(t, 2, requestNumber)
}

func graphQLResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestGateway_PullRequestsForStackUpdate_moreThanNodeLimit(t *testing.T) {
	// Every pull request is in its own native stack,
	// so there are more stack IDs than GitHub accepts
	// in one nodes(ids:) query.
	const count = 150
	var batchSizes []int
	gateway := newTestGateway(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request struct {
			Variables struct {
				IDs []ID `json:"ids"`
			} `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))

		ids := request.Variables.IDs
		if ids == nil {
			prs := make([]string, count)
			for i := range count {
				prs[i] = fmt.Sprintf(`"pr%[1]d": {
					"id": "PR_%[1]d",
					"state": "OPEN",
					"headRefName": "b%[1]d",
					"baseRefName": "main",
					"headRepository": {"owner": {"login": "acme"}, "name": "repo"},
					"stack": {"id": "STACK_%[1]d"}
				}`, i)
			}
			return graphQLResponse(`{"data": {"repository": {` + strings.Join(prs, ",") + `}}}`), nil
		}

		batchSizes = append(batchSizes, len(ids))
		if len(ids) > 100 {
			return graphQLResponse(`{"errors": [{
				"type": "ARGUMENT_LIMIT",
				"message": "You may not provide more than 100 node ids"
			}]}`), nil
		}

		nodes := make([]string, len(ids))
		for i, id := range ids {
			var n int
			_, err := fmt.Sscanf(string(id), "STACK_%d", &n)
			require.NoError(t, err)
			nodes[i] = fmt.Sprintf(`{
				"id": %q,
				"number": %d,
				"entries": {
					"nodes": [{"pullRequest": {"number": %d, "state": "OPEN"}}],
					"pageInfo": {"endCursor": "", "hasNextPage": false}
				}
			}`, id, n, n+1)
		}
		return graphQLResponse(`{"data": {"nodes": [` + strings.Join(nodes, ",") + `]}}`), nil
	}))

	numbers := make([]int, count)
	for i := range numbers {
		numbers[i] = i + 1
	}

	got, err := gateway.PullRequestsForStackUpdate(t.Context(), "acme", "repo", numbers)
	require.NoError(t, err)

	assert.Equal(t, []int{100, 50}, batchSizes)
	require.Len(t, got, count)
	for i, pr := range got {
		require.NotNil(t, pr.Stack, "pull request %d", i)
		assert.Equal(t, i, pr.Stack.Number)
		assert.Equal(t, []PullRequestStackMember{
			{Number: i + 1, State: PullRequestStateOpen},
		}, pr.Stack.Members)
	}
}
