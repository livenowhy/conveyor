package conveyor

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"golang.org/x/oauth2"

	"github.com/fsouza/go-dockerclient"
	"github.com/google/go-github/github"
	"github.com/remind101/empire/pkg/dockerutil"
)

// Context is used for the commit status context.
const Context = "container/docker"

type BuildOptions struct {
	// Repository is the repo to build.
	Repository string
	// Commit is the git commit to build.
	Commit string
	// Branch is the name of the branch that this build relates to.
	Branch string
	// An io.Writer where output will be written to.
	OutputStream io.Writer
}

type Conveyor struct {
	// BuildDir is the directory where repositories will be cloned.
	BuildDir string
	// AuthConfiguration is the docker authentication credentials for
	// pushing and pulling images from the registry.
	AuthConfiguration docker.AuthConfiguration
	// docker client for interacting with the docker daemon api.
	docker *docker.Client
	// github client for creating commit statuses.
	github githubClient
}

// NewFromEnv returns a new Conveyor instance with options configured from the
// environment variables.
func NewFromEnv() (*Conveyor, error) {
	c, err := dockerutil.NewDockerClientFromEnv()
	if err != nil {
		return nil, err
	}

	u, p := os.Getenv("DOCKER_USERNAME"), os.Getenv("DOCKER_PASSWORD")
	auth := docker.AuthConfiguration{
		Username: u,
		Password: p,
	}

	return &Conveyor{
		BuildDir:          os.Getenv("BUILD_DIR"),
		AuthConfiguration: auth,
		github:            newGitHubClient(os.Getenv("GITHUB_TOKEN")),
		docker:            c,
	}, nil
}

// Build builds a docker image for the
func (c *Conveyor) Build(opts BuildOptions) (err error) {
	defer func() {
		status := "success"
		if err != nil {
			status = "error"
		}
		c.updateStatus(opts.Repository, opts.Commit, status)
	}()

	var dir string
	dir, err = ioutil.TempDir(c.BuildDir, opts.Commit)
	if err != nil {
		return fmt.Errorf("tempdir: %v", err)
	}

	if err = c.updateStatus(opts.Repository, opts.Commit, "pending"); err != nil {
		return fmt.Errorf("status: %v", err)
	}

	if err = c.checkout(dir, opts); err != nil {
		return fmt.Errorf("checkout: %v", err)
	}

	if err = c.pull(opts); err != nil {
		return fmt.Errorf("pull: %v", err)
	}

	if _, err = c.build(dir, opts); err != nil {
		return fmt.Errorf("build: %v", err)
	}

	tags := []string{
		opts.Branch,
		opts.Commit,
	}

	if err = c.tag(opts.Repository, tags...); err != nil {
		return fmt.Errorf("tag: %v", err)
	}

	if err = c.push(opts.Repository, opts.OutputStream, append([]string{"latest"}, tags...)...); err != nil {
		return fmt.Errorf("push: %v", err)
	}

	return nil
}

// checkout clones the repo and checks out the given commit.
func (c *Conveyor) checkout(dir string, opts BuildOptions) error {
	cmd := newCommand(opts.OutputStream, "git", "clone", "--depth=50", fmt.Sprintf("--branch=%s", opts.Branch), fmt.Sprintf("git://github.com/%s.git", opts.Repository), dir)
	cmd.Dir = c.BuildDir
	if err := cmd.Run(); err != nil {
		return err
	}

	cmd = newCommand(opts.OutputStream, "git", "checkout", "-qf", opts.Commit)
	cmd.Dir = dir
	return cmd.Run()
}

// pull pulls the last docker image for the branch.
// TODO: try: branch -> latest
func (c *Conveyor) pull(opts BuildOptions) error {
	return c.pullTags(opts.Repository, opts.OutputStream, opts.Branch, "latest")
}

// pullTags attempts to pull each tag. It will return when the first pull
// succeeds or when none of the pulls succeed.
func (c *Conveyor) pullTags(repo string, w io.Writer, tags ...string) (err error) {
	for _, t := range tags {
		if err = c.pullTag(repo, t, w); err != nil {
			if tagNotFound(err) {
				// Try next tag.
				continue
			}
		}
		return
	}

	return
}

func (c *Conveyor) pullTag(repo, tag string, w io.Writer) error {
	return c.docker.PullImage(docker.PullImageOptions{
		Repository:   repo,
		Tag:          tag,
		OutputStream: w,
	}, c.AuthConfiguration)
}

// build builds the docker image.
// TODO: Build using the docker client. We build this by shelling out because
// the docker CLI handles .dockerignore.
func (c *Conveyor) build(dir string, opts BuildOptions) (*docker.Image, error) {
	cmd := newCommand(opts.OutputStream, "docker", "build", "-t", opts.Repository, ".")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	return c.docker.InspectImage(opts.Repository)
}

// push pushes the image to the docker registry.
func (c *Conveyor) push(image string, w io.Writer, tags ...string) error {
	for _, t := range tags {
		if err := c.docker.PushImage(docker.PushImageOptions{
			Name:         image,
			Tag:          t,
			OutputStream: w,
		}, c.AuthConfiguration); err != nil {
			return err
		}
	}

	return nil
}

// tag tags the image id with the given tags.
func (c *Conveyor) tag(image string, tags ...string) error {
	for _, t := range tags {
		if err := c.docker.TagImage(image, docker.TagImageOptions{
			Repo:  image,
			Tag:   t,
			Force: true,
		}); err != nil {
			return err
		}
	}

	return nil
}

// updateStatus updates the given commit with a new status.
func (c *Conveyor) updateStatus(repo, commit, status string) error {
	context := Context
	parts := strings.SplitN(repo, "/", 2)
	_, _, err := c.github.CreateStatus(parts[0], parts[1], commit, &github.RepoStatus{
		State:   &status,
		Context: &context,
	})
	return err
}

// newCommand returns an exec.Cmd that writes to Stdout and Stderr.
func newCommand(w io.Writer, name string, arg ...string) *exec.Cmd {
	cmd := exec.Command(name, arg...)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd
}

var tagNotFoundRegex = regexp.MustCompile(`.*Tag (\S+) not found in repository (\S+)`)

func tagNotFound(err error) bool {
	return tagNotFoundRegex.MatchString(err.Error())
}

// githubClient represents a client that can create github commit statuses.
type githubClient interface {
	CreateStatus(owner, repo, ref string, status *github.RepoStatus) (*github.RepoStatus, *github.Response, error)
}

// newGitHubClient returns a new githubClient instance. If token is an empty
// string, then a fake client will be returned.
func newGitHubClient(token string) githubClient {
	if token == "" {
		return &nullGitHubClient{}
	}

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(oauth2.NoContext, ts)
	return github.NewClient(tc).Repositories
}

// nullGitHubClient is an implementation of the githubClient interface that does
// nothing.
type nullGitHubClient struct{}

func (c *nullGitHubClient) CreateStatus(owner, repo, ref string, status *github.RepoStatus) (*github.RepoStatus, *github.Response, error) {
	fmt.Printf("Updating status of %s on %s/%s to %s\n", ref, owner, repo, *status.State)
	return nil, nil, nil
}