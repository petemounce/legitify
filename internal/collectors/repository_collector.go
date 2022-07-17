package collectors

import (
	"fmt"
	"log"

	"github.com/Legit-Labs/legitify/internal/context_utils"
	"github.com/Legit-Labs/legitify/internal/scorecard"

	"github.com/Legit-Labs/legitify/internal/common/group_waiter"
	"github.com/Legit-Labs/legitify/internal/common/permissions"

	ghclient "github.com/Legit-Labs/legitify/internal/clients/github"
	ghcollected "github.com/Legit-Labs/legitify/internal/collected/github"
	"github.com/Legit-Labs/legitify/internal/common/namespace"
	"github.com/Legit-Labs/legitify/internal/common/utils"
	"github.com/google/go-github/v44/github"
	"github.com/shurcooL/githubv4"
	"golang.org/x/net/context"
)

type repositoryCollector struct {
	baseCollector
	Client           ghclient.Client
	Context          context.Context
	scorecardEnabled bool
}

func newRepositoryCollector(ctx context.Context, client ghclient.Client) collector {
	c := &repositoryCollector{
		Client:           client,
		Context:          ctx,
		scorecardEnabled: context_utils.GetScorecardEnabled(ctx),
	}
	initBaseCollector(&c.baseCollector, c)
	return c
}

func (rc *repositoryCollector) Namespace() namespace.Namespace {
	return namespace.Repository
}

type totalCountRepoQuery struct {
	Organization struct {
		Repositories struct {
			TotalCount githubv4.Int
		} `graphql:"repositories(first: 1)"`
	} `graphql:"organization(login: $login)"`
}

func (rc *repositoryCollector) CollectMetadata() Metadata {
	gw := group_waiter.New()
	orgs, err := rc.Client.CollectOrganizations()

	if err != nil {
		log.Printf("failed to collect organization %s", err)
		return Metadata{}
	}

	var totalCount int32 = 0
	for _, org := range orgs {
		org := org
		gw.Do(func() {
			variables := map[string]interface{}{
				"login": githubv4.String(*org.Login),
			}

			totalCountQuery := totalCountRepoQuery{}

			e := rc.Client.GraphQLClient().Query(rc.Context, &totalCountQuery, variables)

			if e != nil {
				return
			}

			totalCount += int32(totalCountQuery.Organization.Repositories.TotalCount)
		})
	}
	gw.Wait()

	return Metadata{
		TotalEntities: int(totalCount),
	}
}

func (rc *repositoryCollector) Collect() subCollectorChannels {
	return rc.wrappedCollection(func() {
		orgs, err := rc.Client.CollectOrganizations()

		if err != nil {
			log.Printf("failed to collect organizations %s", err)
			return
		}

		rc.totalCollectionChange(0)

		gw := group_waiter.New()
		for _, org := range orgs {
			localOrg := org
			gw.Do(func() {
				_ = utils.Retry(func() (bool, error) {
					err := rc.collectRepositories(&localOrg)
					return true, err
				}, 5, fmt.Sprintf("collect repositories for %s", *localOrg.Login))
			})
		}
		gw.Wait()
	})
}

type repoQuery struct {
	Organization struct {
		Repositories struct {
			PageInfo ghcollected.GitHubQLPageInfo
			Nodes    []ghcollected.GitHubQLRepository
		} `graphql:"repositories(first: 50, after: $repositoryCursor)"`
	} `graphql:"organization(login: $login)"`
}

func (rc *repositoryCollector) collectRepositories(org *ghcollected.ExtendedOrg) error {
	variables := map[string]interface{}{
		"login":            githubv4.String(*org.Login),
		"repositoryCursor": (*githubv4.String)(nil),
	}

	gw := group_waiter.New()
	for {
		query := repoQuery{}
		err := rc.Client.GraphQLClient().Query(rc.Context, &query, variables)

		if err != nil {
			return err
		}

		gw.Do(func() {
			nodes := query.Organization.Repositories.Nodes
			extraGw := group_waiter.New()
			for i := range nodes {
				node := &(nodes[i])
				extraGw.Do(func() {
					repo := rc.collectExtraData(org, node)
					entityName := fullRepoName(*org.Login, repo.Repository.Name)
					missingPermissions := rc.checkMissingPermissions(repo, entityName)
					rc.issueMissingPermissions(missingPermissions...)
					rc.collectData(*org, repo, repo.Repository.Url, []permissions.Role{org.Role, repo.Repository.ViewerPermission})
					rc.collectionChangeByOne()
				})
			}
			extraGw.Wait()
		})

		if !query.Organization.Repositories.PageInfo.HasNextPage {
			break
		}

		variables["repositoryCursor"] = query.Organization.Repositories.PageInfo.EndCursor
	}
	gw.Wait()

	return nil
}

func (rc *repositoryCollector) collectExtraData(org *ghcollected.ExtendedOrg, repository *ghcollected.GitHubQLRepository) ghcollected.Repository {
	var err error
	repo := ghcollected.Repository{
		Repository: repository,
	}
	login := *org.Login

	repo, err = rc.getVulnerabilityAlerts(repo, login)
	if err != nil {
		// If we can't get vulnerability alerts, rego will ignore it (as nil)
		log.Printf("error getting vulnerability alerts for %s: %s", fullRepoName(login, repo.Repository.Name), err)
	}

	repo, err = rc.getRepositoryHooks(repo, login)
	if err != nil {
		log.Printf("error getting repository hooks for %s: %s", fullRepoName(login, repo.Repository.Name), err)
	}

	repo, err = rc.getRepoCollaborators(repo, login)
	if err != nil {
		log.Printf("error getting repository collaborators for %s: %s", fullRepoName(login, repo.Repository.Name), err)
	}

	// free plan doesn't support branch protection unless it's a public repository
	if !repo.Repository.IsPrivate || !org.IsFree() {
		repo, err = rc.fixBranchProtectionInfo(repo, login)
		if err != nil {
			// If we can't get branch protection info, rego will ignore it (as nil)
			log.Printf("error getting branch protection info for %s: %s", repository.Name, err)
		}
	} else {
		perm := newMissingPermission(permissions.RepoAdmin, fullRepoName(login, repo.Repository.Name), orgIsFreeEffect, namespace.Repository)
		rc.issueMissingPermissions(perm)
	}

	if rc.scorecardEnabled {
		scResult, err := scorecard.Calculate(rc.Context, repository.Url, repo.Repository.IsPrivate)
		if err != nil {
			scResult = nil
			log.Printf("error getting scorecard result for %s: %s", repository.Name, err)
		}
		repo.Scorecard = scResult
	}

	return repo
}

func (rc *repositoryCollector) getRepositoryHooks(repo ghcollected.Repository, org string) (ghcollected.Repository, error) {
	var result []*github.Hook

	err := ghclient.PaginateResults(func(opts *github.ListOptions) (*github.Response, error) {
		hooks, resp, err := rc.Client.Client().Repositories.ListHooks(rc.Context, org, repo.Repository.Name, opts)
		if err != nil {
			if resp.Response.StatusCode == 404 {
				perm := newMissingPermission(permissions.RepoHookRead, fullRepoName(org, repo.Repository.Name),
					"Cannot read repository webhooks", namespace.Repository)
				rc.issueMissingPermissions(perm)
			}
			return nil, err
		}

		result = append(result, hooks...)

		return resp, nil
	})

	if err != nil {
		return repo, err
	}

	repo.Hooks = result
	return repo, nil
}

func (rc *repositoryCollector) getVulnerabilityAlerts(repo ghcollected.Repository, org string) (ghcollected.Repository, error) {
	enabled, _, err := rc.Client.Client().Repositories.GetVulnerabilityAlerts(rc.Context, org, repo.Repository.Name)

	if err != nil {
		return repo, err
	}

	repo.VulnerabilityAlertsEnabled = &enabled

	return repo, nil
}

func (rc *repositoryCollector) getRepoCollaborators(repo ghcollected.Repository, org string) (ghcollected.Repository, error) {
	users, _, err := rc.Client.Client().Repositories.ListCollaborators(rc.Context, org, repo.Repository.Name, &github.ListCollaboratorsOptions{})

	if err != nil {
		return repo, err
	}

	repo.Collaborators = users

	return repo, nil
}

// fixBranchProtectionInfo fixes the branch protection info for the repository,
// to reflect whether there is no branch protection, or just no permission to fetch the info.
func (rc *repositoryCollector) fixBranchProtectionInfo(repository ghcollected.Repository, org string) (ghcollected.Repository, error) {
	if repository.Repository.DefaultBranchRef == nil {
		return repository, nil // no branches
	}
	if repository.Repository.DefaultBranchRef.BranchProtectionRule != nil {
		return repository, nil // branch protection info already available
	}

	repoName := repository.Repository.Name
	branchName := *repository.Repository.DefaultBranchRef.Name
	_, _, err := rc.Client.Client().Repositories.GetBranchProtection(rc.Context, org, repoName, branchName)
	if err == nil {
		log.Printf("incosistent permissions (GitHub bug): graphQL query failed, but branch protection info is available. Ignoring\n")
		return repository, nil
	}

	isNoPermErr := func(err error) bool {
		// Inspired by github.isBranchNotProtected()
		const noPermMessage = "Not Found"
		errorResponse, ok := err.(*github.ErrorResponse)
		return ok && errorResponse.Message == noPermMessage
	}

	switch {
	case isNoPermErr(err):
		repository.NoBranchProtectionPermission = true
	case err == github.ErrBranchNotProtected:
		// Already the default value for the NoBranchProtectionPerm & BranchProtectionRule fields
	default: // Any other error is an operational error
		return repository, err
	}

	return repository, nil
}

func (rc *repositoryCollector) checkMissingPermissions(repo ghcollected.Repository, entityName string) []missingPermission {
	missingPermissions := []missingPermission{}
	if repo.NoBranchProtectionPermission {
		effect := "Cannot read repository branch protection information"
		perm := newMissingPermission(permissions.RepoAdmin, entityName, effect, namespace.Repository)
		missingPermissions = append(missingPermissions, perm)
	}
	return missingPermissions
}

const (
	orgIsFreeEffect = "Branch protection cannot be collected because the organization is in free plan"
)