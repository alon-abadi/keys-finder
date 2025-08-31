package main

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const repoURL = "https://github.com/alon-abadi/sample-keys-repo.git"
const cloneDir = "./sample-keys-repo"

type Job struct {
	Commit *object.Commit
}

type Hit struct {
	File     string
	Pattern  string
	Line     string
	Change   string // "added"/"deleted"
}

// Result = all matches for one commit
type Result struct {
	Commit   *object.Commit
	Branches []string
	Hits     []Hit
}

func main() {
	defer os.RemoveAll(cloneDir)
	_ = os.RemoveAll(cloneDir)

	repo, err := git.PlainClone(cloneDir, false, &git.CloneOptions{URL: repoURL})
	if err != nil {
		log.Fatalf("clone: %v", err)
	}

	// Fetch all branches/tags
	if err := repo.Fetch(&git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{"refs/*:refs/*"},
		Tags:       git.AllTags,
		Force:      true,
	}); err != nil && err != git.NoErrAlreadyUpToDate {
		log.Fatalf("fetch: %v", err)
	}

	// Collect unique commits + map of commit hash -> branch names
	commits, branchMap := collectAllCommits(repo)

	// Sort by author date (descending)
	sort.Slice(commits, func(i, j int) bool {
		return commits[i].Author.When.After(commits[j].Author.When)
	})
	fmt.Printf("Collected %d unique commits across %d branch heads\n", len(commits), len(branchMap))

	
	reAKIA := regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	reASIA := regexp.MustCompile(`ASIA[0-9A-Z]{16}`)
	reSecret40 := regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9/+=])([A-Za-z0-9/+=]{40})(?:[^A-Za-z0-9/+=]|$)`)

	matchers := []*regexp.Regexp{reAKIA, reASIA, reSecret40}

	// Channels
	jobs := make(chan Job, 50)
	results := make(chan Result, 50)

	// Start workers
	numWorkers := runtime.NumCPU()
	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go worker(repo, jobs, results, matchers, branchMap, &wg)
	}

	// Printer goroutine
	var pwg sync.WaitGroup
	pwg.Add(1)
	go func() {
		defer pwg.Done()
		for res := range results {
			printResult(res)
		}
	}()

	// Feed jobs sequentially
	for _, c := range commits {
		jobs <- Job{Commit: c}
	}
	close(jobs)

	// Wait for workers, then close results
	wg.Wait()
	close(results)
	pwg.Wait()

	fmt.Println("Done.")
}

// collectAllCommits returns commits + map of commit hash -> branch names that reach it
func collectAllCommits(repo *git.Repository) ([]*object.Commit, map[string][]string) {
	refs, err := repo.References()
	if err != nil {
		log.Fatalf("references: %v", err)
	}
	seen := make(map[string]bool)
	var result []*object.Commit
	branchMap := make(map[string][]string)

	err = refs.ForEach(func(ref *plumbing.Reference) error {
		// only remote branches; skip symbolic origin/HEAD
		if !ref.Name().IsRemote() || ref.Name().String() == "refs/remotes/origin/HEAD" {
			return nil
		}
		branch := strings.TrimPrefix(ref.Name().Short(), "origin/")

		iter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
		if err != nil {
			return nil
		}
		defer iter.Close()

		return iter.ForEach(func(c *object.Commit) error {
			h := c.Hash.String()
			if !seen[h] {
				seen[h] = true
				result = append(result, c)
			}
			branchMap[h] = appendUnique(branchMap[h], branch)
			return nil
		})
	})
	if err != nil {
		log.Fatalf("iterate refs: %v", err)
	}
	return result, branchMap
}

func appendUnique(list []string, item string) []string {
	for _, v := range list {
		if v == item {
			return list
		}
	}
	return append(list, item)
}

// Worker: gets a commit, diffs vs parent, scans chunks for matches (adds + deletes)
func worker(repo *git.Repository, jobs <-chan Job, results chan<- Result, matchers []*regexp.Regexp, branchMap map[string][]string, wg *sync.WaitGroup) {
	defer wg.Done()
	for job := range jobs {
		c := job.Commit

		// Diff commit vs parent (first parent for merges)
		var parentTree *object.Tree
		if c.NumParents() > 0 {
			if p, err := c.Parent(0); err == nil {
				parentTree, _ = p.Tree()
			}
		}
		thisTree, err := c.Tree()
		if err != nil {
			continue
		}

		var patch *object.Patch
		if parentTree == nil {
			patch, err = thisTree.Patch(nil)
		} else {
			patch, err = parentTree.Patch(thisTree)
		}
		if err != nil {
			continue
		}

		var hits []Hit
		for _, fp := range patch.FilePatches() {
			from, to := fp.Files()
			path := "(unknown)"
			if to != nil {
				path = to.Path() // prefer new path (handles renames)
			} else if from != nil {
				path = from.Path()
			}

			for _, ch := range fp.Chunks() {
				var change string
				switch ch.Type() {
				case diff.Add:
					change = "added"
				case diff.Delete:
					change = "deleted"
				default:
					continue // skip equal/context
				}

				lines := strings.Split(ch.Content(), "\n")
				for _, ln := range lines {
					if len(ln) < 10 {
						continue
					}
					for _, re := range matchers {
						if re.MatchString(ln) {
							pat := re.String()
							if re == matchers[2] { // the 40-char secret rule
								pat = "AWS secret (40 chars)"
							}
							hits = append(hits, Hit{
								File:    path,
								Pattern: pat,
								Line:    strings.TrimSpace(ln),
								Change:  change,
							})
							break
						}
					}
				}
			}
		}

		if len(hits) > 0 {
			results <- Result{
				Commit:   c,
				Branches: branchMap[c.Hash.String()],
				Hits:     hits,
			}
		}
	}
}

func printResult(res Result) {
	c := res.Commit
	fmt.Printf("\n----- MATCH -----\n")
	fmt.Printf("commit %s\nAuthor: %s <%s>\nDate:   %s\nBranches: %s\n    %s\n",
		c.Hash.String(),
		c.Author.Name, c.Author.Email,
		c.Author.When.Format("2006-01-02 15:04:05 -0700"),
		strings.Join(res.Branches, ", "),
		firstLine(c.Message),
	)
	for _, h := range res.Hits {
		preview := h.Line
		if len(preview) > 200 {
			preview = preview[:200] + "…"
		}
		// Prefix with +/- style hint based on change kind
		prefix := "+"
		if h.Change == "deleted" {
			prefix = "-"
		}
		fmt.Printf("  %s  (%s) [matched %s]\n    %s %s\n", h.File, h.Change, h.Pattern, prefix, preview)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
