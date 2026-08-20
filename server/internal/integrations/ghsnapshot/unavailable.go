package ghsnapshot

import (
	"context"
	"errors"
)

// UnavailableClient keeps the scan job registered when GitHub App configuration
// is absent or invalid, so eligible workspaces record FAILED plans instead of
// being misreported as uninitialized.
type UnavailableClient struct {
	err error
}

func NewUnavailableClient(err error) *UnavailableClient {
	if err == nil {
		err = errors.New("github app is not configured")
	}
	return &UnavailableClient{err: err}
}

func (c *UnavailableClient) ResolveRepositoryAccess(context.Context, []int64, string, string) (int64, error) {
	return 0, c.err
}

func (c *UnavailableClient) InstallationToken(context.Context, int64) (string, error) {
	return "", c.err
}

func (c *UnavailableClient) Head(context.Context, string, string, string, string) (string, error) {
	return "", c.err
}

func (c *UnavailableClient) Page(context.Context, string, string, string, string, string, int, int) ([]CommitMeta, error) {
	return nil, c.err
}
