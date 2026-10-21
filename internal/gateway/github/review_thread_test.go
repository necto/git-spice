package github

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGateway_PullRequestReviewThreadCounts(t *testing.T) {
	requestNum := 0
	gateway := newTestGateway(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request struct {
			Query     string         `json:"query"`
			Variables jsontext.Value `json:"variables"`
		}
		require.NoError(t, json.UnmarshalRead(r.Body, &request))

		requestNum++
		var response string
		switch requestNum {
		case 1:
			assert.Equal(t, "query($ids:[ID!]!){nodes(ids: $ids){... on PullRequest{reviewThreads(first: 2){totalCount,pageInfo{endCursor,hasNextPage},nodes{isResolved}}}}}", request.Query)
			assert.JSONEq(t, `{
				"ids": ["PR_1", "PR_2"]
			}`, string(request.Variables))
			response = `{
				"data": {"nodes": [
					{"reviewThreads": {
						"totalCount": 3,
						"nodes": [{"isResolved": true}, {"isResolved": false}],
						"pageInfo": {"endCursor": "next", "hasNextPage": true}
					}},
					{"reviewThreads": {
						"totalCount": 1,
						"nodes": [{"isResolved": false}],
						"pageInfo": {"endCursor": "", "hasNextPage": false}
					}}
				]}
			}`
		case 2:
			assert.Equal(t, "query($after:String!$first:Int!$id:ID!){node(id: $id){... on PullRequest{reviewThreads(first: $first, after: $after){totalCount,pageInfo{endCursor,hasNextPage},nodes{isResolved}}}}}", request.Query)
			assert.JSONEq(t, `{
				"after": "next",
				"first": 2,
				"id": "PR_1"
			}`, string(request.Variables))
			response = `{
				"data": {"node": {"reviewThreads": {
					"totalCount": 3,
					"nodes": [{"isResolved": true}],
					"pageInfo": {"endCursor": "", "hasNextPage": false}
				}}}
			}`
		default:
			t.Fatalf("unexpected request %d", requestNum)
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(response)),
		}, nil
	}))
	got, err := gateway.PullRequestReviewThreadCounts(
		t.Context(), []ID{"PR_1", "PR_2"},
		&PaginationOptions{ItemsPerPage: 2},
	)
	require.NoError(t, err)
	assert.Equal(t, []*ReviewThreadCounts{
		{Total: 3, Resolved: 2},
		{Total: 1, Resolved: 0},
	}, got)
	assert.Equal(t, 2, requestNum)
}

func TestGateway_PullRequestReviewThreadCounts_rejectsMismatchedNodeCount(t *testing.T) {
	gateway := newResponseGateway(t, `{
		"data": {"nodes": [
			{"reviewThreads": {
				"totalCount": 1,
				"nodes": [{"isResolved": true}],
				"pageInfo": {"endCursor": "", "hasNextPage": false}
			}}
		]}
	}`)

	_, err := gateway.PullRequestReviewThreadCounts(
		t.Context(), []ID{"PR_1", "PR_2"}, nil,
	)
	assert.ErrorContains(t, err, "got 1 nodes for 2 IDs")
}

func TestGateway_PullRequestReviewThreadCounts_moreThanNodeLimit(t *testing.T) {
	// GitHub rejects nodes(ids:) queries with more than 100 IDs.
	var batchSizes []int
	gateway := newTestGateway(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request struct {
			Variables struct {
				IDs []ID `json:"ids"`
			} `json:"variables"`
		}
		require.NoError(t, json.UnmarshalRead(r.Body, &request))

		ids := request.Variables.IDs
		batchSizes = append(batchSizes, len(ids))
		if len(ids) > 100 {
			return graphQLResponse(`{"errors": [{
				"type": "ARGUMENT_LIMIT",
				"message": "You may not provide more than 100 node ids"
			}]}`), nil
		}

		// Report each ID's numeric suffix as its thread count to check ordering.
		nodes := make([]string, len(ids))
		for i, id := range ids {
			var n int
			_, err := fmt.Sscanf(string(id), "PR_%d", &n)
			require.NoError(t, err)
			nodes[i] = fmt.Sprintf(`{"reviewThreads": {
				"totalCount": %d,
				"nodes": [],
				"pageInfo": {"endCursor": "", "hasNextPage": false}
			}}`, n)
		}
		return graphQLResponse(`{"data": {"nodes": [` + strings.Join(nodes, ",") + `]}}`), nil
	}))

	ids := make([]ID, 150)
	for i := range ids {
		ids[i] = ID(fmt.Sprintf("PR_%d", i))
	}

	got, err := gateway.PullRequestReviewThreadCounts(t.Context(), ids, nil)
	require.NoError(t, err)

	assert.Equal(t, []int{100, 50}, batchSizes)
	require.Len(t, got, len(ids))
	for i, counts := range got {
		assert.Equal(t, i, counts.Total)
	}
}
