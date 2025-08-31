package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ----- Config -----
const repoURL = "https://github.com/alon-abadi/sample-keys-repo.git"
const cloneDir = "./sample-keys-repo"
const statePath = cloneDir + "/.scan_state.json"
const scanDeletionsToo = true

type State struct {
	Processed map[string]bool `json:"processed"` // commit -> done?
}

type Job struct {
	Index    int
	Commit   *object.Commit
	Branches []string
	Chunks   []ChunkLines // pre-extracted changed lines (no repo access needed in workers)
}

type ChunkLines struct {
	File   string
	Change string   // "added" or "deleted"
	Lines  []string // content lines in this chunk
}

type Hit struct {
	File    string
	Pattern string
	Line    string
	Change  string // "added" or "deleted"
}

type Result struct {
	Index    int
	Commit   *object.Commit
	Branches []string
	Hits     []Hit
	Err      error
}

// ----- Main -----
func main() {
	// Handle Ctrl+C / SIGTERM to allow graceful stop (state is saved after each commit).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Open or clone repo (keep folder for resume)
	repo := openOrCloneRepo(cloneDir, repoURL)

	// Fetch latest refs
	if err := repo.Fetch(&git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{"refs/*:refs/*"},
		Tags:       git.AllTags,
		Force:      true,
	}); err != nil && err != git.NoErrAlreadyUpToDate {
		log.Fatalf("fetch: %v", err)
	}

	// Collect unique commits + commit->[]branches
	commits, branchMap := collectAllCommits(repo)

	sort.Slice(commits, func(i, j int) bool {
		ti, tj := commits[i].Author.When, commits[j].Author.When
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return commits[i].Hash.String() > commits[j].Hash.String()
	})


	st := loadState(statePath)
	if st.Processed == nil {
		st.Processed = make(map[string]bool)
	}

	// Filter out processed commits
	toScan := make([]*object.Commit, 0, len(commits))
	skipped := 0
	for _, c := range commits {
		if st.Processed[c.Hash.String()] {
			skipped++
			continue
		}
		toScan = append(toScan, c)
	}

	fmt.Printf("Total commits: %d | already processed: %d | to scan now: %d\n",
		len(commits), skipped, len(toScan))

	
	reAKIA := regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	reASIA := regexp.MustCompile(`ASIA[0-9A-Z]{16}`)
	reSecret40 := regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9/+=])([A-Za-z0-9/+=]{40})(?:[^A-Za-z0-9/+=]|$)`)
	matchers := []*regexp.Regexp{reAKIA, reASIA, reSecret40}

	
	jobs := make(chan Job, 64)
	results := make(chan Result, 64)

	numWorkers := runtime.NumCPU()
	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go worker(jobs, results, matchers, &wg)
	}

	var collWG sync.WaitGroup
	collWG.Add(1)
	go func() {
		defer collWG.Done()
		for res := range results {
			markProcessedAndSave(statePath, st, res.Commit.Hash.String())

			if res.Err != nil {
				log.Printf("scan error for %s: %v", res.Commit.Hash, res.Err)
				continue
			}
			if len(res.Hits) == 0 {
				continue
			}
			printResult(res)
		}
	}()

	// enqueue jobs
  for i, c := range toScan {
      if ctx.Err() != nil { // stop cleanly on Ctrl+C / SIGTERM
          fmt.Println("Signal received, stopping enqueue…")
          break
      }
      chunks := extractChangedLinesForCommit(c)
      jobs <- Job{
          Index:    i,
          Commit:   c,
          Branches: branchMap[c.Hash.String()],
          Chunks:   chunks,
      }
  }
  close(jobs)

	wg.Wait()
	close(results)
	collWG.Wait()

	fmt.Println("Scan finished.")
}

func openOrCloneRepo(dir, url string) *git.Repository {
	// If folder exists & looks like a repo, open it; else clone.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		repo, err := git.PlainOpen(dir)
		if err != nil {
			log.Fatalf("open repo: %v", err)
		}
		return repo
	}
	_ = os.MkdirAll(dir, 0755)
	repo, err := git.PlainClone(dir, false, &git.CloneOptions{
		URL: url,
	})
	if err != nil {
		log.Fatalf("clone failed: %v", err)
	}
	return repo
}

func collectAllCommits(repo *git.Repository) ([]*object.Commit, map[string][]string) {
	refs, err := repo.References()
	if err != nil {
		log.Fatalf("references: %v", err)
	}
	seen := make(map[string]bool)
	var result []*object.Commit
	branchMap := make(map[string][]string)

	err = refs.ForEach(func(ref *plumbing.Reference) error {
		// Only remote branches; skip the symbolic origin/HEAD
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

func extractChangedLinesForCommit(c *object.Commit) []ChunkLines {
	var parentTree *object.Tree
	if c.NumParents() > 0 {
		if p, err := c.Parent(0); err == nil {
			parentTree, _ = p.Tree()
		}
	}
	thisTree, err := c.Tree()
	if err != nil {
		return nil
	}

	var patch *object.Patch
	if parentTree == nil {
		patch, err = thisTree.Patch(nil)
	} else {
		patch, err = parentTree.Patch(thisTree)
	}
	if err != nil || patch == nil {
		return nil
	}

	var out []ChunkLines
	for _, fp := range patch.FilePatches() {
		from, to := fp.Files()
		path := "(unknown)"
		if to != nil {
			path = to.Path()
		} else if from != nil {
			path = from.Path()
		}

		for _, ch := range fp.Chunks() {
			switch ch.Type() {
			case diff.Add:
				lines := strings.Split(ch.Content(), "\n")
				out = append(out, ChunkLines{File: path, Change: "added", Lines: lines})
			case diff.Delete:
				if scanDeletionsToo {
					lines := strings.Split(ch.Content(), "\n")
					out = append(out, ChunkLines{File: path, Change: "deleted", Lines: lines})
				}
			default:
				// diff.Equal (context) — ignore
			}
		}
	}
	return out
}

func worker(jobs <-chan Job, results chan<- Result, matchers []*regexp.Regexp, wg *sync.WaitGroup) {
	defer wg.Done()
	for job := range jobs {
		var hits []Hit

		for _, chunk := range job.Chunks {
			for _, ln := range chunk.Lines {
				if len(ln) < 10 {
					continue
				}
				for _, re := range matchers {
					if re.MatchString(ln) {
						pat := re.String()
						if re == matchers[2] {
							pat = "AWS secret (40 chars)"
						}
						hits = append(hits, Hit{
							File:    chunk.File,
							Pattern: pat,
							Line:    strings.TrimSpace(ln),
							Change:  chunk.Change,
						})
						break
					}
				}
			}
		}

		results <- Result{
			Index:    job.Index,
			Commit:   job.Commit,
			Branches: job.Branches,
			Hits:     hits,
			Err:      nil,
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
		prefix := "+"
		if h.Change == "deleted" {
			prefix = "-"
		}
		fmt.Printf("  %s  (%s) [matched %s]\n    %s %s\n",
			h.File, h.Change, h.Pattern, prefix, preview)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func loadState(path string) *State {
	data, err := os.ReadFile(path)
	if err != nil {
		return &State{Processed: make(map[string]bool)}
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil || st.Processed == nil {
		return &State{Processed: make(map[string]bool)}
	}
	return &st
}

var stateMu sync.Mutex

func markProcessedAndSave(path string, st *State, hash string) {
	stateMu.Lock()
	defer stateMu.Unlock()

	if st.Processed == nil {
		st.Processed = make(map[string]bool)
	}
	if st.Processed[hash] {
		return // already recorded
	}
	st.Processed[hash] = true

	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		log.Printf("state mkdir: %v", err)
		return
	}
	f, err := os.Create(tmp)
	if err != nil {
		log.Printf("state create: %v", err)
		return
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		log.Printf("state encode: %v", err)
		return
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		log.Printf("state fsync: %v", err)
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		log.Printf("state close: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		log.Printf("state rename: %v", err)
		return
	}
}
