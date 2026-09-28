package api

import (
	"context"
	"fmt"
)

// VariableList is what an environment runs with, and whether it has it yet.
//
// Pending is the control plane's answer to "I set it and nothing changed":
// the values were saved but the site has not been restarted with them, which
// happens on the next deploy or on `var apply`. A pointer for the reason
// Variable.Description is one: a control plane older than the field does not
// send it, and nil ("cannot say") is a different answer from false ("nothing
// pending"), which a script reading --format json should be able to tell.
type VariableList struct {
	Pending   *bool      `json:"pending"`
	Variables []Variable `json:"variables"`
}

// VariablesApplied is the answer to `var apply`.
//
// Environments by slug. Applied are restarting with their variables;
// unreachable could not be sent them just now, and go out with their next
// deploy instead.
type VariablesApplied struct {
	Applied     []string `json:"applied"`
	Unreachable []string `json:"unreachable"`
}

// ApplyEnvironmentVariables restarts an environment's site with its variables
// now, rather than with the next deploy.
func (c *Client) ApplyEnvironmentVariables(ctx context.Context, environmentID int) (*VariablesApplied, error) {
	return c.applyVariables(ctx, fmt.Sprintf("/environments/%d/variables/apply", environmentID))
}

// ApplyProjectVariables does the same for every environment of a project
// still running on values the project has since changed.
func (c *Client) ApplyProjectVariables(ctx context.Context, projectID int) (*VariablesApplied, error) {
	return c.applyVariables(ctx, fmt.Sprintf("/projects/%d/variables/apply", projectID))
}

func (c *Client) applyVariables(ctx context.Context, path string) (*VariablesApplied, error) {
	var out VariablesApplied

	if err := c.post(ctx, path, nil, &out); err != nil {
		return nil, err
	}

	return &out, nil
}
