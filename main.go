package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/barkimedes/go-deepcopy"
	"github.com/fishworks/gofish"
	"github.com/go-git/go-git/v5"
	"github.com/google/go-github/v39/github"
	"github.com/spf13/afero"
	"github.com/yuin/gluamapper"
	lua "github.com/yuin/gopher-lua"
	"golang.org/x/oauth2"
)

type Options struct {
	Rig     string
	Skip    map[string]bool
	Release map[string]GithubRelease

	AuthorName  string
	AuthorEmail string

	GithubAuthToken string

	GithubClient *github.Client
	GithubRegex  *regexp.Regexp
	FoodPath     string
}

func main() {
	ctx := context.Background()

	auth := ""
	rig := "https://github.com/fishworks/fish-food"
	skip := ""
	release := `consul:hashicorp/consul,kubectl:kubernetes/kubernetes,nomad:hashicorp/nomad,terraform:hashicorp/terraform,vagrant:hashicorp/vagrant,vault:hashicorp/vault`

	skipMap, err := skipToMap(skip)
	if err != nil {
		log.Fatal(err)
	}
	releaseMap, err := releaseToMap(release)
	if err != nil {
		log.Fatal(err)
	}

	opts := Options{
		Rig:     rig,
		Skip:    skipMap,
		Release: releaseMap,

		AuthorName:  "arbourd",
		AuthorEmail: "arbourd@users.noreply.github.com",

		GithubAuthToken: auth,
	}

	count, err := run(ctx, opts)
	if err != nil {
		log.Fatal(err)
	}
	os.Exit(count)
}

func skipToMap(skip string) (map[string]bool, error) {
	m := map[string]bool{}
	if len(skip) == 0 {
		return m, nil
	}

	re := regexp.MustCompile(`[\w-_]+`)

	for _, food := range strings.Split(strings.TrimSuffix(skip, ","), ",") {
		if !re.MatchString(food) {
			return m, fmt.Errorf("validate skip: did not match spec `food`: %s", food)
		}
		m[food] = true
	}
	return m, nil
}

type GithubRelease struct {
	Org  string
	Repo string
}

func releaseToMap(release string) (map[string]GithubRelease, error) {
	m := map[string]GithubRelease{}
	if len(release) == 0 {
		return m, nil
	}

	re := regexp.MustCompile(`[\w-_]+:[\w-_]+/[\w-_]+`)

	for _, food := range strings.Split(strings.TrimSuffix(release, ","), ",") {
		if !re.MatchString(food) {
			return m, fmt.Errorf("validate release: did not match spec `food:org/repo`: %s", food)
		}

		org := strings.Split(strings.Split(food, ":")[1], "/")[0]
		repo := strings.Split(strings.Split(food, ":")[1], "/")[1]
		m[strings.Split(food, ":")[0]] = GithubRelease{Org: org, Repo: repo}
	}

	return m, nil
}

func run(ctx context.Context, opts Options) (int, error) {
	opts.GithubClient = github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: opts.GithubAuthToken})))
	opts.GithubRegex = regexp.MustCompile(`https://github\.com\/(?P<org>[\w-_]+)/(?P<repo>[\w-_]+)`)

	dir, err := ioutil.TempDir("", "gfb_")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(dir)

	_, err = git.PlainClone(dir, false, &git.CloneOptions{
		URL:   opts.Rig,
		Depth: 1,
	})
	if err != nil {
		return 1, err
	}
	opts.FoodPath = filepath.Join(dir, "Food")

	feed, err := getFood(opts.FoodPath)
	if err != nil {
		return 1, err
	}

	errc := 0
	for _, f := range feed {
		err := processFood(ctx, f, opts)
		if err != nil {
			errc += 1
			log.Printf("ERROR: %s: %v\n", f.Name, err)
		}
	}

	return errc, nil
}

func releaseURL(f gofish.Food, rmap map[string]GithubRelease) string {
	if release, ok := rmap[f.Name]; ok {
		return fmt.Sprintf("https://github.com/%s/%s", release.Org, release.Repo)
	}
	if strings.HasPrefix(f.Packages[0].URL, "https://github.com/") {
		return f.Packages[0].URL
	}
	if strings.HasPrefix(f.Homepage, "https://github.com/") {
		return f.Homepage
	}

	return ""
}

func processFood(ctx context.Context, f gofish.Food, opts Options) error {
	if opts.Skip[f.Name] {
		log.Println("WARN: " + f.Name + ": skipping")
		return nil
	}

	if strings.Contains(f.Name, "@") {
		log.Println("WARN: " + f.Name + ": skipping pinned version")
		return nil
	}

	url := releaseURL(f, opts.Release)
	if len(url) == 0 {
		log.Println("WARN: " + f.Name + ": no available github release")
		return nil
	}

	results := opts.GithubRegex.FindAllStringSubmatch(url, -1)
	org := results[0][1]
	repo := results[0][2]

	release, _, err := opts.GithubClient.Repositories.GetLatestRelease(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("github release: %w", err)
	}

	version, err := semver.NewVersion(f.Version)
	if err != nil {
		return fmt.Errorf("semver: %w", err)
	}

	newVersion, err := semver.NewVersion(*release.TagName)
	if err != nil {
		log.Println("WARN: " + f.Name + ": cannot parse semver for: " + *release.TagName)
		return nil
	}

	c, err := semver.NewConstraint("> " + version.String())
	if err != nil {
		return fmt.Errorf("semver: %w", err)
	}

	if !c.Check(newVersion) {
		return nil
	}
	log.Println("updating: " + f.Name + " " + newVersion.String())

	food, err := copyFood(f)
	if err != nil {
		return fmt.Errorf("copying food: %w", err)
	}
	food.Version = newVersion.String()

	for i, pkg := range food.Packages {
		newURL := strings.ReplaceAll(pkg.URL, f.Version, food.Version)
		sha, err := getSHA(newURL)
		if err != nil {
			return err
		}

		food.Packages[i].URL = newURL
		food.Packages[i].SHA256 = sha
	}

	// Update lua
	foodFilePath := filepath.Join(opts.FoodPath, f.Name+".lua")
	fs := afero.NewOsFs()
	info, err := fs.Stat(foodFilePath)
	if err != nil {
		return fmt.Errorf("finding info of file %s: %w", foodFilePath, err)
	}
	mode := info.Mode()

	foodBytes, err := afero.ReadFile(fs, foodFilePath)
	if err != nil {
		return fmt.Errorf("reading file %s: %w", foodFilePath, err)
	}

	updatedFood := strings.ReplaceAll(string(foodBytes), f.Version, food.Version)
	for i, p := range f.Packages {
		updatedFood = strings.ReplaceAll(updatedFood, p.SHA256, food.Packages[i].SHA256)
	}

	err = afero.WriteFile(fs, foodFilePath, []byte(updatedFood), mode)
	if err != nil {
		return fmt.Errorf("writing to file %s: %w", foodFilePath, err)
	}

	// Lint
	errs := food.Lint()
	if len(errs) > 0 {
		var e error
		for _, err := range errs {
			e = fmt.Errorf("%w", err)
		}
		return fmt.Errorf("linting:\n - '%w'", e)
	}

	return nil
}

func getFood(foodPath string) ([]gofish.Food, error) {
	var feed []gofish.Food

	files, err := ioutil.ReadDir(foodPath)
	if err != nil {
		return feed, err
	}

	for _, f := range files {
		L := lua.NewState()
		if err := L.DoFile(foodPath + "/" + f.Name()); err != nil {
			return feed, err
		}

		var food gofish.Food
		if err := gluamapper.Map(L.GetGlobal("food").(*lua.LTable), &food); err != nil {
			return feed, err
		}

		feed = append(feed, food)
	}

	return feed, nil
}

func copyFood(f gofish.Food) (gofish.Food, error) {
	f2, err := deepcopy.Anything(f)
	if err != nil {
		return f, err
	}

	return f2.(gofish.Food), nil
}

func getSHA(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("downloading package to calculate shasum: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		respBody, _ := ioutil.ReadAll(resp.Body)
		return "", fmt.Errorf("downloading package %v\n\n"+"response code: %v\nresponse body: %v", url, resp.StatusCode, string(respBody))
	} else if resp.StatusCode >= 400 {
		return "", fmt.Errorf("downloading package %v\n\n"+"response code: %v", url, resp.StatusCode)
	}

	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", fmt.Errorf("downloading package: %v", err)
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
