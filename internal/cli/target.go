package cli

import (
	"context"
	"os"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/resolve"
)

// Target is the project and environment a command acts on.
type Target struct {
	Project     *api.Project
	Environment *api.Environment
}

// Resolver builds a resolver from the configuration and the flags.
//
// The precedence is settled here, once, for both nouns: a flag beats the
// environment, which beats the config file, and a checkout answers for
// itself where none of them said anything. See internal/resolve for the two
// rules that hold below that.
func (e *Env) Resolver(client resolve.Client) (*resolve.Resolver, error) {
	cfg, err := e.Config()
	if err != nil {
		return nil, err
	}

	dir, err := os.Getwd()
	if err != nil {
		// Not fatal. Without a directory there is no git to read and the
		// resolution falls back to the flags, which is the same place it
		// would be outside a checkout.
		dir = ""
	}

	return &resolve.Resolver{
		Client:          client,
		Dir:             dir,
		ProjectHint:     firstNonEmpty(e.flagProject, cfg.Project),
		EnvironmentHint: firstNonEmpty(e.flagEnvironment, cfg.Environment),
	}, nil
}

// ResolveEnvironment settles which environment a command means.
//
// positional is the command's own optional argument, which is the most
// explicit thing there is and wins over every hint.
func (e *Env) ResolveEnvironment(ctx context.Context, positional string) (*Target, error) {
	client, err := e.Client()
	if err != nil {
		return nil, err
	}

	resolver, err := e.Resolver(client)
	if err != nil {
		return nil, err
	}

	project, err := resolver.Project(ctx)
	if err != nil {
		return nil, err
	}

	environment, err := resolver.Environment(ctx, project, positional)
	if err != nil {
		return nil, err
	}

	return &Target{Project: project, Environment: environment}, nil
}

// ResolveProject settles which project a command means.
func (e *Env) ResolveProject(ctx context.Context) (*api.Project, error) {
	client, err := e.Client()
	if err != nil {
		return nil, err
	}

	resolver, err := e.Resolver(client)
	if err != nil {
		return nil, err
	}

	return resolver.Project(ctx)
}

// Detail reads the full environment, which is where the SSH target lives.
func (e *Env) Detail(ctx context.Context, target *Target) (*api.EnvironmentDetail, error) {
	client, err := e.Client()
	if err != nil {
		return nil, err
	}

	return client.Environment(ctx, target.Environment.ID)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}
