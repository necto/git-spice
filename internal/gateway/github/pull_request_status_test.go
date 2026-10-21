package github

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGateway_ChangeStatuses(t *testing.T) {
	gateway := newResponseGateway(t, `{
		"data": {"nodes": [{"state": "OPEN", "headRefOid": "abc"}]}
	}`)
	statuses, err := gateway.ChangeStatuses(t.Context(), []ID{"PR_1"})
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, PullRequestStateOpen, statuses[0].State)
}

func TestGateway_ChangeStatuses_moreThanNodeLimit(t *testing.T) {
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
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body: io.NopCloser(strings.NewReader(`{"errors": [{
					"type": "ARGUMENT_LIMIT",
					"message": "You may not provide more than 100 node ids"
				}]}`)),
			}, nil
		}

		// Echo each ID back as its head OID to check ordering.
		nodes := make([]string, len(ids))
		for i, id := range ids {
			nodes[i] = fmt.Sprintf(`{"state": "OPEN", "headRefOid": %q}`, id)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body: io.NopCloser(strings.NewReader(
				`{"data": {"nodes": [` + strings.Join(nodes, ",") + `]}}`,
			)),
		}, nil
	}))

	ids := make([]ID, 150)
	for i := range ids {
		ids[i] = ID(fmt.Sprintf("PR_%d", i))
	}

	statuses, err := gateway.ChangeStatuses(t.Context(), ids)
	require.NoError(t, err)

	assert.Equal(t, []int{100, 50}, batchSizes)
	require.Len(t, statuses, len(ids))
	for i, status := range statuses {
		assert.Equal(t, string(ids[i]), status.HeadRefOID)
	}
}
