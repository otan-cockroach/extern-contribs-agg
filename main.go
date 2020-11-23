package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/google/go-github/v30/github"
)

var flagOrganization = flag.String(
	"organization",
	"cockroachdb",
	"organization to look under",
)
var flagAuthorsOrg = flag.String(
	"authors_organization",
	"cockroachdb",
	"source of authors org",
)
var flagAuthorsRepo = flag.String(
	"authors_repo",
	"cockroach",
	"source of authors repo",
)
var flagAuthorsPath = flag.String(
	"authors_path",
	"AUTHORS",
	"source of authors files",
)
var flagRepos = flag.String(
	"repos",
	"cockroach,pebble,docs,activerecord-cockroachdb-adapter,cockroach-go,cockroach-operator,django-cockroachdb,sequelize-cockroachdb,sqlalchemy-cockroachdb",
	"repos to lookup, comma separated",
)
var flagIntermediateOutput = flag.String(
	"intermediate_output_file",
	"intermediate_output.json",
	"place where intermediate output is placed",
)
var flagOutput = flag.String(
	"output",
	"output.md",
	"output file",
)
var flagUseIntermediate = flag.Bool(
	"use_intermediate",
	false,
	"if true, generates output from pre-generated intermediate output file",
)
var flagBlocklist = flag.String(
	"blocklist",
	"petermattis-square,craig[bot],nigeltao,dependabot,dependabot[bot],alimi,timgraham,papb,chrislovecnm,marlabrizel,rkruze",
	"comma separated list of people to exclude",
)

func getOrganizationLogins(
	ctx context.Context, ghClient *github.Client, org string,
) (map[string]*github.User, error) {
	logins := make(map[string]*github.User)
	opts := &github.ListMembersOptions{
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}
	more := true
	for more {
		members, resp, err := ghClient.Organizations.ListMembers(
			ctx,
			org,
			opts,
		)
		if err != nil {
			return nil, errors.Newf("error listing org members: %v", err)
		}
		for _, member := range members {
			logins[member.GetLogin()] = member
		}
		more = resp.NextPage != 0
		if more {
			opts.Page = resp.NextPage
		}
	}
	return logins, nil
}

func getRepositories(ctx context.Context, ghClient *github.Client) []*github.Repository {
	opts := &github.RepositoryListByOrgOptions{}
	more := true
	var repos []*github.Repository
	for more {
		add, resp, err := ghClient.Repositories.ListByOrg(
			ctx,
			*flagOrganization,
			opts,
		)
		if err != nil {
			panic(err)
		}
		repos = append(repos, add...)
		more = resp.NextPage != 0
		if more {
			opts.Page = resp.NextPage
		}
	}
	return repos
}

func getOrganizationEmailsAndNamesFromAuthors(
	ctx context.Context, ghClient *github.Client,
) (map[string]struct{}, map[string]struct{}) {
	authorsFile, _, _, err := ghClient.Repositories.GetContents(
		ctx,
		*flagAuthorsOrg,
		*flagAuthorsRepo,
		*flagAuthorsPath,
		nil,
	)
	if err != nil {
		panic(err)
	}
	retEmails := map[string]struct{}{}
	retLogins := map[string]struct{}{}
	contents, err := authorsFile.GetContent()
	if err != nil {
		panic(err)
	}
	lines := strings.Split(contents, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "@cockroachlabs.com") {
			continue
		}
		fields := strings.Split(line, " ")
		seenEmail := false
		for i, field := range fields {
			if strings.HasPrefix(field, "<") && strings.HasSuffix(field, ">") {
				if !seenEmail {
					retLogins[strings.Join(fields[:i], " ")] = struct{}{}
				}
				seenEmail = true
				email := field[1 : len(field)-1]
				retEmails[email] = struct{}{}
			}
		}
	}
	return retEmails, retLogins
}

type user struct {
	userURL string
	login   string
	name    string
	times   []time.Time
}

func formatContributors(users map[string]user, from time.Time, to time.Time) string {
	timesByUser := map[string]int{}
	for u, obj := range users {
		for _, t := range obj.times {
			if t.After(from) && t.Before(to) {
				timesByUser[u] = timesByUser[u] + 1
			}
		}
	}
	type toSortEntry struct {
		u     user
		count int
	}
	var toSort []toSortEntry
	for u, c := range timesByUser {
		toSort = append(toSort, toSortEntry{u: users[u], count: c})
	}
	sort.Slice(toSort, func(i, j int) bool {
		if toSort[i].count == toSort[j].count {
			return toSort[i].u.login < toSort[j].u.login
		}
		return toSort[i].count > toSort[j].count
	})

	var ret []string
	total := 0
	for _, entry := range toSort {
		total += entry.count
		ret = append(
			ret,
			fmt.Sprintf("[%s](%s) (%d)", entry.u.name, entry.u.userURL, entry.count),
		)
	}
	return fmt.Sprintf("%d contributors, %d commits\n\n", len(toSort), total) + strings.Join(ret, ", ")
}

func intermediateOutputToOutput(ctx context.Context, ghClient *github.Client) {
	inFile, err := os.Open(*flagIntermediateOutput)
	if err != nil {
		panic(err)
	}
	defer func() { _ = inFile.Close() }()
	read, err := ioutil.ReadAll(inFile)
	if err != nil {
		panic(err)
	}
	var usersIn map[string][]string
	if err := json.Unmarshal(read, &usersIn); err != nil {
		panic(err)
	}

	users := map[string]user{}
	resultCh := make(chan user, len(usersIn))
	const userRateLimit = 20
	rateLimit := make(chan struct{}, userRateLimit)
	for i := 0; i < userRateLimit; i++ {
		rateLimit <- struct{}{}
	}
	blocklisted := map[string]struct{}{}
	for _, blocked := range strings.Split(*flagBlocklist, ",") {
		blocklisted[blocked] = struct{}{}
	}
	var wg sync.WaitGroup
	for u, timesIn := range usersIn {
		wg.Add(1)
		go func(u string, timesIn []string) {
			defer func() {
				wg.Done()
				rateLimit <- struct{}{}
			}()
			<-rateLimit
			fmt.Printf("** looking up %s\n", u)
			ghUser, _, err := ghClient.Users.Get(ctx, u)
			if err != nil {
				panic(err)
			}
			times := []time.Time{}
			for _, tIn := range timesIn {
				t, err := time.Parse(time.RFC3339, tIn)
				if err != nil {
					panic(err)
				}
				times = append(times, t)
			}
			name := ghUser.GetName()
			if name == "" {
				name = u
			}
			resultCh <- user{
				userURL: ghUser.GetHTMLURL(),
				login:   u,
				name:    name,
				times:   times,
			}
		}(u, timesIn)
	}

	_, blocklistedNames := getOrganizationEmailsAndNamesFromAuthors(ctx, ghClient)

	wg.Wait()
	for i := 0; i < len(usersIn); i++ {
		u := <-resultCh
		if _, ok := blocklisted[u.login]; ok {
			continue
		}
		if _, ok := blocklistedNames[u.name]; ok {
			continue
		}
		users[u.login] = u
	}

	fromRepos := []string{}
	for _, repo := range strings.Split(*flagRepos, ",") {
		fromRepos = append(
			fromRepos,
			fmt.Sprintf("[%s](https://github.com/%s/%s)", repo, *flagOrganization, repo),
		)
	}

	out := fmt.Sprintf(
		`
Last generated at %s.

Contributions from: %s.

# All-Time External Contributors

%s

# By Year
`,
		time.Now().Format(time.RFC3339),
		strings.Join(fromRepos, ", "),
		formatContributors(users, time.Date(2014, 1, 1, 0, 0, 0, 0, time.UTC), time.Now()),
	)
	for year := time.Now().Year(); year >= 2014; year-- {
		out += fmt.Sprintf(
			`## %d

%s

`,
			year,
			formatContributors(
				users,
				time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC),
				time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC),
			),
		)
	}

	fmt.Printf("%s\n", out)
	outFile, err := os.OpenFile(*flagOutput, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		panic(err)
	}
	if _, err := outFile.Write([]byte(out)); err != nil {
		panic(err)
	}
	if err := outFile.Close(); err != nil {
		panic(err)
	}
	fmt.Printf("* Output to %q\n", *flagOutput)
}

func main() {
	flag.Parse()

	ctx := context.Background()
	ghClient, err := getGithubClient()
	if err != nil {
		panic(err)
	}

	if *flagUseIntermediate {
		intermediateOutputToOutput(ctx, ghClient)
		return
	}

	organizationMembers, err := getOrganizationLogins(ctx, ghClient, *flagOrganization)
	if err != nil {
		panic(err)
	}

	emails, names := getOrganizationEmailsAndNamesFromAuthors(ctx, ghClient)

	// Go through each repo.
	users := map[string]*github.User{}
	userTimes := map[string][]time.Time{}

	for _, repo := range strings.Split(*flagRepos, ",") {
		fmt.Printf("* Looking at repo %s\n", repo)
		opts := &github.CommitsListOptions{
			ListOptions: github.ListOptions{
				PerPage: 1000,
			},
		}
		more := true
		for more {
			commits, resp, err := ghClient.Repositories.ListCommits(
				ctx,
				*flagOrganization,
				repo,
				opts,
			)
			if err != nil {
				panic(err)
			}
			for _, commit := range commits {
				if len(commit.GetCommit().Parents) > 0 {
					continue
				}
				if commit.GetAuthor().GetLogin() == "" {
					continue
				}
				if _, ok := organizationMembers[commit.GetAuthor().GetLogin()]; ok {
					continue
				}
				if strings.Contains(commit.GetCommit().GetAuthor().GetEmail(), "@cockroachlabs.com") {
					continue
				}
				if strings.HasPrefix(commit.GetCommit().GetMessage(), "Merge pull request ") {
					continue
				}
				if _, ok := names[commit.GetAuthor().GetName()]; ok {
					continue
				}
				if _, ok := emails[commit.GetCommit().GetAuthor().GetEmail()]; ok {
					continue
				}
				fmt.Printf(
					"* found commit by %s (%s)) on %s\n",
					commit.GetAuthor().GetLogin(),
					commit.GetCommit().GetAuthor().GetEmail(),
					commit.Commit.GetAuthor().GetDate().Format(time.RFC3339),
				)
				users[commit.GetAuthor().GetLogin()] = commit.GetAuthor()
				userTimes[commit.GetAuthor().GetLogin()] = append(
					userTimes[commit.GetAuthor().GetLogin()],
					commit.Commit.GetAuthor().GetDate(),
				)
			}
			more = resp.NextPage != 0
			if more {
				opts.Page = resp.NextPage
			}
		}
	}

	intermediateOutputFile, err := os.OpenFile(*flagIntermediateOutput, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		panic(err)
	}
	commitTimes := map[string][]string{}
	for user, times := range userTimes {
		for _, t := range times {
			commitTimes[user] = append(commitTimes[user], t.Format(time.RFC3339))
		}
	}
	b, err := json.Marshal(commitTimes)
	if err != nil {
		panic(err)
	}
	if _, err := intermediateOutputFile.Write(b); err != nil {
		panic(err)
	}
	if err := intermediateOutputFile.Close(); err != nil {
		panic(err)
	}

	intermediateOutputToOutput(ctx, ghClient)
}
