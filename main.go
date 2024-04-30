package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os/exec"
	"strings"

	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
)

type pullRequests []pullRequest

type branches []string
type pullRequest struct {
	Number int
	Merged bool
	State  githubv4.PullRequestState
}

func main() {

	// Get flags
	safeMode := flag.Bool("safe", false, "Enable safe mode")
	forceMode := flag.Bool("force", false, "Enable deleting closed branches, not just merged")
	flag.Parse()

	// Create context
	ctx := context.Background()

	// Get token from GH CLI
	err, token := getToken()
	if err != nil {
		fmt.Printf("Failed to get current Github repo: %v\n", err)
		return
	}

	client := getGraphqlClient(token, ctx)

	owner, repo, defaultBranch, err := getCurrentGithubRepo()
	if err != nil {
		fmt.Printf("Failed to get current Github repo: %v\n", err)
		return
	}

	// Getting local git branches
	branchList, err := getBranches()
	if err != nil {
		fmt.Printf("Failed to get branches: %v\n", err)
		return
	}

	// Sanitise the branches
	sanitisedBranches := branchList.sanitiseBranches(defaultBranch)

	for _, branch := range sanitisedBranches {

		prs, err := getAllPullRequests(ctx, client, owner, repo, branch)
		if err != nil {
			fmt.Printf("Error getting pull requests for branch %s: %v\n", branch, err)
			return
		}

		if prs == nil {
			fmt.Printf("No pull requests found for branch %s\n", branch)
			continue
		}

		anyPrsClosed := prs.areAnyPRsClosed()
		noPrsOpen := !prs.areAnyPRsOpen()

		if anyPrsClosed && *forceMode {
			fmt.Printf("Deleting branch `%s` even with closed pull requests\n", branch)
		}

		canDeleteBranch := prs.areAllPRsMerged() || (anyPrsClosed && noPrsOpen && *forceMode)
		if canDeleteBranch {
			deleteBranch(branch, *safeMode)
		} else {
			if !noPrsOpen {
				fmt.Printf("Branch %s has open pull requests: %v\n", branch, prs.getUnmergedPrUrls(owner, repo))
			}
			if anyPrsClosed {
				fmt.Printf("Branch %s has closed pull requests: %v\n", branch, prs.getClosedPrUrls(owner, repo))
				fmt.Printf("Use -force flag to delete branches with closed pull requests\n")
			}
		}
	}
}

func getBranches() (branches, error) {
	cmd := exec.Command("git", "branch", "-l")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	branchList := strings.Split(string(output), "\n")
	return branchList, err
}

func getGraphqlClient(token string, ctx context.Context) *githubv4.Client {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := githubv4.NewClient(tc)
	return client
}

func getToken() (error, string) {
	tokenBytes, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return err, ""
	}
	token := strings.TrimSpace(string(tokenBytes[:]))
	return err, token
}

func deleteBranch(branch string, safeMode bool) {
	fmt.Printf("Deleting branch: %s\n", branch)
	if safeMode {
		fmt.Printf("Safe mode enabled, skipping deletion...\n")
	} else {
		deleteCmd := exec.Command("git", "branch", "-D", branch)
		if err := deleteCmd.Run(); err != nil {
			fmt.Printf("Failed to delete branch %s: %v\n", branch, err)
		}
	}
}

func getCurrentGithubRepo() (string, string, string, error) {
	type GithubRepoOutput struct {
		Name             string `json:"name"`
		DefaultBranchRef struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
		Owner struct {
			ID    string `json:"id"`
			Login string `json:"login"`
		} `json:"owner"`
	}

	cmd := exec.Command("gh", "repo", "view", "--json", "owner,name,defaultBranchRef")
	output, err := cmd.Output()
	if err != nil {
		return "", "", "", err
	}

	var repo GithubRepoOutput
	err = json.Unmarshal(output, &repo)
	if err != nil {
		return "", "", "", err
	}

	return repo.Owner.Login, repo.Name, repo.DefaultBranchRef.Name, nil
}

func getAllPullRequests(ctx context.Context, client *githubv4.Client, owner string, repo string, branch string) (pullRequests, error) {
	var query struct {
		Repository struct {
			PullRequests struct {
				Nodes    pullRequests
				PageInfo struct {
					EndCursor   githubv4.String
					HasNextPage bool
				}
			} `graphql:"pullRequests(headRefName: $branchName, first: 100, after: $cursor)"` // 100 per page.
		} `graphql:"repository(owner: $repositoryOwner, name: $repositoryName)"`
	}
	variables := map[string]interface{}{
		"repositoryOwner": githubv4.String(owner),
		"repositoryName":  githubv4.String(repo),
		"branchName":      githubv4.String(branch),
		"cursor":          (*githubv4.String)(nil), // Null after argument to get first page.
	}

	var allPullRequests []pullRequest

	for {
		err := client.Query(ctx, &query, variables)
		if err != nil {
			return nil, err
		}
		if query.Repository.PullRequests.Nodes == nil {
			break
		}
		allPullRequests = append(allPullRequests, query.Repository.PullRequests.Nodes...)

		if !query.Repository.PullRequests.PageInfo.HasNextPage {
			break
		}
		variables["cursor"] = githubv4.NewString(query.Repository.PullRequests.PageInfo.EndCursor)
	}

	return allPullRequests, nil
}

func (p pullRequests) areAllPRsMerged() bool {
	for _, pr := range p {
		if !pr.Merged {
			return false
		}
	}
	return true
}

func (p pullRequests) areAnyPRsClosed() bool {
	for _, pr := range p {
		if pr.State == "CLOSED" {
			return true
		}
	}
	return false
}

func (p pullRequests) areAnyPRsOpen() bool {
	for _, pr := range p {
		if pr.State == "OPEN" {
			return true
		}
	}
	return false
}

func (p pullRequests) getUnmergedPrUrls(owner string, repo string) []string {
	var prUrls = make([]string, 0)
	for _, pr := range p {
		if !pr.Merged {
			prUrls = append(prUrls, fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, pr.Number))
		}
	}
	return prUrls
}

func (p pullRequests) getClosedPrUrls(owner string, repo string) []string {
	var prUrls = make([]string, 0)
	for _, pr := range p {
		if pr.State == "CLOSED" {
			prUrls = append(prUrls, fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, pr.Number))
		}
	}
	return prUrls
}

func (b branches) sanitiseBranches(defaultBranch string) branches {
	var returnBranches = make(branches, 0)
	for _, branchVal := range b {
		branch := strings.TrimSpace(strings.TrimPrefix(branchVal, "* "))
		if branch == "" || branch == defaultBranch {
			continue
		}
		returnBranches = append(returnBranches, branch)
	}
	return returnBranches
}
