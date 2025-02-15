package pulp

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/redhatinsights/edge-api/pkg/ptr"
)

func (ps *PulpService) RepositoriesCreate(ctx context.Context, name string) (*OstreeOstreeRepositoryResponse, error) {
	body := OstreeOstreeRepository{
		Name:         name,
		ComputeDelta: ptr.To(true),
	}
	resp, err := ps.cwr.RepositoriesOstreeOstreeCreateWithResponse(ctx, ps.dom, body, addAuthenticationHeader)

	if err != nil {
		return nil, err
	}

	if resp.JSON201 == nil {
		return nil, fmt.Errorf("unexpected response: %d, body: %s", resp.StatusCode(), string(resp.Body))
	}

	return resp.JSON201, nil
}

// RepositoriesImport imports an initial commit into a repo
func (ps *PulpService) RepositoriesImport(ctx context.Context, id uuid.UUID, repoName, artifactHref string) (*OstreeOstreeRepositoryResponse, error) {
	body := OstreeImportAll{
		Artifact:       artifactHref,
		RepositoryName: repoName,
	}
	resp, err := ps.cwr.RepositoriesOstreeOstreeImportAllWithResponse(ctx, ps.dom, id, body, addAuthenticationHeader)

	if err != nil {
		return nil, err
	}

	if resp.JSON202 == nil {
		return nil, fmt.Errorf("unexpected response: %d, body: %s", resp.StatusCode(), string(resp.Body))
	}

	hrefs, err := ps.WaitForTask(ctx, resp.JSON202.Task)
	if err != nil {
		return nil, err
	}
	if len(hrefs) != 1 {
		return nil, fmt.Errorf("unexpected number of created resources: %d", len(hrefs))
	}
	href := hrefs[0]

	result, err := ps.RepositoriesRead(ctx, ScanUUID(&href))
	if err != nil {
		return nil, err
	}

	return result, nil
}

// RepositoriesImportCommit updates an existing repo containing one or more commits
func (ps *PulpService) RepositoriesImportCommit(ctx context.Context, id uuid.UUID, repoName, artifactHref string, ostreeRef string) (*OstreeOstreeRepositoryResponse, error) {
	body := OstreeImportCommitsToRef{
		Artifact:       artifactHref,
		Ref:            ostreeRef,
		RepositoryName: repoName,
	}

	// OstreeImportCommits includes the OSTree ref for updating a tree that has at least one commit
	// Use OstreeImportAll for the initial commit
	// see https://pulpproject.org/pulp_ostree/docs/user/guides/import-commit/
	resp, err := ps.cwr.RepositoriesOstreeOstreeImportCommitsWithResponse(ctx, ps.dom, id, body, addAuthenticationHeader)
	if err != nil {
		return nil, err
	}

	if resp.JSON202 == nil {
		return nil, fmt.Errorf("unexpected response: %d, body: %s", resp.StatusCode(), string(resp.Body))
	}

	hrefs, err := ps.WaitForTask(ctx, resp.JSON202.Task)
	if err != nil {
		return nil, err
	}
	if len(hrefs) != 1 {
		return nil, fmt.Errorf("unexpected number of created resources: %d", len(hrefs))
	}
	href := hrefs[0]

	result, err := ps.RepositoriesRead(ctx, ScanUUID(&href))
	if err != nil {
		return nil, err
	}

	return result, nil
}

func (ps *PulpService) RepositoriesRead(ctx context.Context, id uuid.UUID) (*OstreeOstreeRepositoryResponse, error) {
	req := RepositoriesOstreeOstreeReadParams{}
	resp, err := ps.cwr.RepositoriesOstreeOstreeReadWithResponse(ctx, ps.dom, id, &req, addAuthenticationHeader)

	if err != nil {
		return nil, err
	}

	if resp.JSON200 == nil {
		return nil, fmt.Errorf("unexpected response: %d, body: %s", resp.StatusCode(), string(resp.Body))
	}

	return resp.JSON200, nil
}

func (ps *PulpService) RepositoriesDelete(ctx context.Context, id uuid.UUID) error {
	resp, err := ps.cwr.RepositoriesOstreeOstreeDelete(ctx, ps.dom, id, addAuthenticationHeader)

	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode != 204 {
		return fmt.Errorf("unexpected response: %d", resp.StatusCode)
	}

	return nil
}
