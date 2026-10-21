package github

import (
	"context"
	"fmt"
	"slices"
)

// _maxNodeIDs is the most node IDs GitHub accepts in one nodes(ids:) query.
const _maxNodeIDs = 100

// ChangeStatus is GitHub's compact state and head revision for a pull request.
type ChangeStatus struct {
	// State is the pull request lifecycle state.
	State PullRequestState `json:"state"`

	// HeadRefOID is the Git object ID at the head of the pull request.
	HeadRefOID string `json:"headRefOid"`
}

// ChangeStatuses loads compact pull request states in node ID order.
//
// IDs are queried in batches of at most [_maxNodeIDs].
func (c *Gateway) ChangeStatuses(ctx context.Context, ids []ID) ([]*ChangeStatus, error) {
	query := compactGraphQL(`
		query($ids:[ID!]!){
			nodes(ids: $ids){
				... on PullRequest{state,headRefOid}
			}
		}
	`)

	statuses := make([]*ChangeStatus, 0, len(ids))
	for batch := range slices.Chunk(ids, _maxNodeIDs) {
		var result struct {
			Nodes []*ChangeStatus `json:"nodes"`
		}
		if err := c.executeGQL(ctx, query, struct {
			IDs []ID `json:"ids"`
		}{batch}, &result); err != nil {
			return nil, fmt.Errorf("query change statuses: %w", err)
		}
		statuses = append(statuses, result.Nodes...)
	}
	return statuses, nil
}
