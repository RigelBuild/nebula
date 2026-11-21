// SPDX-License-Identifier: MIT OR Apache-2.0

package linear

import (
	"context"
	"fmt"
)

// UpdateDescription replaces an issue description and requires a successful mutation result.
func (c *IssueClient) UpdateDescription(ctx context.Context, identifier, description string) error {
	const query = `mutation UpdateDescription($id: String!, $description: String!) { issueUpdate(id: $id, input: { description: $description }) { success } }`
	var response struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := c.request(ctx, query, map[string]any{"id": identifier, "description": description}, &response); err != nil {
		return fmt.Errorf("updating Linear issue %s description: %w", identifier, err)
	}
	if !response.IssueUpdate.Success {
		return fmt.Errorf("issueUpdate returned success=false for %s", identifier)
	}
	return nil
}
