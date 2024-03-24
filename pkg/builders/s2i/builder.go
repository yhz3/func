package s2i

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/docker/docker/api/types"
	dockerClient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/openshift/source-to-image/pkg/api"
	"github.com/openshift/source-to-image/pkg/api/validation"
	"github.com/openshift/source-to-image/pkg/build"
	"github.com/openshift/source-to-image/pkg/build/strategies"
	s2idocker "github.com/openshift/source-to-image/pkg/docker"
	"github.com/openshift/source-to-image/pkg/scm/git"
	"golang.org/x/exp/maps"
	"golang.org/x/term"

	"knative.dev/func/pkg/builders"
	"knative.dev/func/pkg/docker"
	fn "knative.dev/func/pkg/functions"
)

// DefaultName when no WithName option is provided to NewBuilder
const DefaultName = builders.S2I

var DefaultNodeBuilder = "registry.access.redhat.com/ubi8/nodejs-16-minimal"
var DefaultQuarkusBuilder = "registry.access.redhat.com/ubi8/openjdk-21"
var DefaultPythonBuilder = "registry.access.redhat.com/ubi8/python-39"

// DefaultBuilderImages for s2i builders indexed by Runtime Language
var DefaultBuilderImages = map[string]string{
	"node":       DefaultNodeBuilder,
	"nodejs":     DefaultNodeBuilder,
	"typescript": DefaultNodeBuilder,
	"quarkus":    DefaultQuarkusBuilder,
	"python":     DefaultPythonBuilder,
}

// DockerClient is subset of dockerClient.CommonAPIClient required by this package
type DockerClient interface {
	ImageBuild(ctx context.Context, context io.Reader, options types.ImageBuildOptions) (types.ImageBuildResponse, error)
	ImageInspectWithRaw(ctx context.Context, image string) (types.ImageInspect, []byte, error)
}

// Builder of functions using the s2i subsystem.
type Builder struct {
	name    string
	verbose bool
	impl    build.Builder // S2I builder implementation (aka "Strategy")
	cli     DockerClient
}

type Option func(*Builder)

func WithName(n string) Option {
	return func(b *Builder) {
		b.name = n
	}
}

// WithVerbose toggles verbose logging.
func WithVerbose(v bool) Option {
	return func(b *Builder) {
		b.verbose = v
	}
}

// WithImpl sets an optional S2I Builder implementation override to use in
// place of what will be generated by the S2I build "strategy" system based
// on the config.  Used for mocking the implementation during tests.
func WithImpl(s build.Builder) Option {
	return func(b *Builder) {
		b.impl = s
	}
}

func WithDockerClient(cli DockerClient) Option {
	return func(b *Builder) {
		b.cli = cli
	}
}

// NewBuilder creates a new instance of a Builder with static defaults.
func NewBuilder(options ...Option) *Builder {
	b := &Builder{name: DefaultName}
	for _, o := range options {
		o(b)
	}
	return b
}

// Build the function using the S2I builder.
//
// Platforms:
// The S2I builder supports at most a single platform to target, and the
// platform specified must be available in the provided builder image.
// If the provided builder image is not a multi-architecture image index
// container, specifying a target platform is redundant, so if provided it
// must match that of the single-architecture container or the request is
// invalid.
func (b *Builder) Build(ctx context.Context, f fn.Function, platforms []fn.Platform) (err error) {

	// Builder image from the function if defined, default otherwise.
	builderImage, err := BuilderImage(f, b.name)
	if err != nil {
		return
	}
	// If a platform was requestd
	if len(platforms) == 1 {
		platform := strings.ToLower(platforms[0].OS + "/" + platforms[0].Architecture)
		// Try to get the platform image from within the builder image
		// Will also succeed if the builder image is a single-architecture image
		// and the requested platform matches.
		if builderImage, err = docker.GetPlatformImage(builderImage, platform); err != nil {
			return fmt.Errorf("cannot get platform image reference for %q: %w", platform, err)
		}
	} else if len(platforms) > 1 {
		// Only a single requestd platform supported.
		return errors.New("the S2I builder currently only supports specifying a single target platform")
	}

	// TODO this function currently doesn't support private s2i builder images since credentials are not set

	// Build Config
	cfg := &api.Config{}
	cfg.Quiet = !b.verbose
	cfg.Tag = f.Build.Image
	cfg.Source = &git.URL{URL: url.URL{Path: f.Root}, Type: git.URLTypeLocal}
	cfg.BuilderImage = builderImage
	cfg.BuilderPullPolicy = api.DefaultBuilderPullPolicy
	cfg.PreviousImagePullPolicy = api.DefaultPreviousImagePullPolicy
	cfg.RuntimeImagePullPolicy = api.DefaultRuntimeImagePullPolicy
	cfg.DockerConfig = s2idocker.GetDefaultDockerConfig()

	tmp, err := os.MkdirTemp("", "s2i-build")
	if err != nil {
		return fmt.Errorf("cannot create temporary dir for s2i build: %w", err)
	}
	defer os.RemoveAll(tmp)

	funcignorePath := filepath.Join(f.Root, ".funcignore")
	if _, err := os.Stat(funcignorePath); err == nil {
		s2iignorePath := filepath.Join(f.Root, ".s2iignore")

		// If the .s2iignore file exists, remove it
		if _, err := os.Stat(s2iignorePath); err == nil {
			err := os.Remove(s2iignorePath)
			if err != nil {
				return fmt.Errorf("error removing existing s2iignore file: %w", err)
			}
		}
		// Create the symbolic link
		err = os.Symlink(funcignorePath, s2iignorePath)
		if err != nil {
			return fmt.Errorf("error creating symlink: %w", err)
		}
		// Removing the symbolic link at the end of the function
		defer os.Remove(s2iignorePath)
	}

	cfg.AsDockerfile = filepath.Join(tmp, "Dockerfile")

	var client = b.cli
	if client == nil {
		var c dockerClient.CommonAPIClient
		c, _, err = docker.NewClient(dockerClient.DefaultDockerHost)
		if err != nil {
			return fmt.Errorf("cannot create docker client: %w", err)
		}
		defer c.Close()
		client = c
	}

	scriptURL, err := s2iScriptURL(ctx, client, cfg.BuilderImage)
	if err != nil {
		return fmt.Errorf("cannot get s2i script url: %w", err)
	}
	cfg.ScriptsURL = scriptURL

	// Excludes
	// Do not include .git, .env, .func or any language-specific cache directories
	// (node_modules, etc) in the tar file sent to the builder, as this both
	// bloats the build process and can cause unexpected errors in the resultant
	// function.
	cfg.ExcludeRegExp = "(^|/)\\.git|\\.env|\\.func|node_modules(/|$)"

	// Environment variables
	// Build Envs have local env var references interpolated then added to the
	// config as an S2I EnvironmentList struct
	envs, err := fn.Interpolate(f.Build.BuildEnvs)
	if err != nil {
		return err
	}
	for k, v := range envs {
		cfg.Environment = append(cfg.Environment, api.EnvironmentSpec{Name: k, Value: v})
	}

	// Validate the config
	if errs := validation.ValidateConfig(cfg); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "ERROR: %s\n", e)
		}
		return errors.New("Unable to build via the s2i builder.")
	}

	var impl = b.impl
	// Create the S2I builder instance if not overridden
	if impl == nil {
		impl, _, err = strategies.Strategy(nil, cfg, build.Overrides{})
		if err != nil {
			return fmt.Errorf("cannot create s2i builder: %w", err)
		}
	}

	// Perform the build
	result, err := impl.Build(cfg)
	if err != nil {
		return
	}

	if b.verbose {
		for _, message := range result.Messages {
			fmt.Println(message)
		}
	}

	pr, pw := io.Pipe()

	// s2i apparently is not excluding the files in --as-dockerfile mode
	exclude := regexp.MustCompile(cfg.ExcludeRegExp)

	const up = ".." + string(os.PathSeparator)
	go func() {
		tw := tar.NewWriter(pw)
		err := filepath.Walk(tmp, func(path string, fi fs.FileInfo, err error) error {
			if err != nil {
				return err
			}

			p, err := filepath.Rel(tmp, path)
			if err != nil {
				return fmt.Errorf("cannot get relative path: %w", err)
			}
			if p == "." {
				return nil
			}

			p = filepath.ToSlash(p)

			if exclude.MatchString(p) {
				return nil
			}

			lnk := ""
			if fi.Mode()&fs.ModeSymlink != 0 {
				lnk, err = os.Readlink(path)
				if err != nil {
					return fmt.Errorf("cannot read link: %w", err)
				}
				if filepath.IsAbs(lnk) {
					lnk, err = filepath.Rel(tmp, lnk)
					if err != nil {
						return fmt.Errorf("cannot get relative path for symlink: %w", err)
					}
					if strings.HasPrefix(lnk, up) || lnk == ".." {
						return fmt.Errorf("link %q points outside source root", p)
					}
				}
			}

			hdr, err := tar.FileInfoHeader(fi, lnk)
			if err != nil {
				return fmt.Errorf("cannot create tar header: %w", err)
			}
			hdr.Name = p

			if runtime.GOOS == "windows" {
				// Windows does not have execute permission, we assume that all files are executable.
				hdr.Mode |= 0111
			}

			err = tw.WriteHeader(hdr)
			if err != nil {
				return fmt.Errorf("cannot write header to thar stream: %w", err)
			}
			if fi.Mode().IsRegular() {
				var r io.ReadCloser
				r, err = os.Open(path)
				if err != nil {
					return fmt.Errorf("cannot open source file: %w", err)
				}
				defer r.Close()

				_, err = io.Copy(tw, r)
				if err != nil {
					return fmt.Errorf("cannot copy file to tar stream :%w", err)
				}
			}

			return nil
		})
		_ = tw.Close()
		_ = pw.CloseWithError(err)
	}()

	opts := types.ImageBuildOptions{
		Tags:       []string{f.Build.Image},
		PullParent: true,
	}

	resp, err := client.ImageBuild(ctx, pr, opts)
	if err != nil {
		return fmt.Errorf("cannot build the app image: %w", err)
	}
	defer resp.Body.Close()

	var out io.Writer = io.Discard
	if b.verbose {
		out = os.Stderr
	}

	var isTerminal bool
	var fd uintptr
	if outF, ok := out.(*os.File); ok {
		fd = outF.Fd()
		isTerminal = term.IsTerminal(int(outF.Fd()))
	}

	return jsonmessage.DisplayJSONMessagesStream(resp.Body, out, fd, isTerminal, nil)
}

func s2iScriptURL(ctx context.Context, cli DockerClient, image string) (string, error) {
	img, _, err := cli.ImageInspectWithRaw(ctx, image)
	if err != nil {
		if dockerClient.IsErrNotFound(err) { // image is not in the daemon, get info directly from registry
			var (
				ref name.Reference
				img v1.Image
				cfg *v1.ConfigFile
			)

			ref, err = name.ParseReference(image)
			if err != nil {
				return "", fmt.Errorf("cannot parse image name: %w", err)
			}
			if _, ok := ref.(name.Tag); ok && !slices.Contains(maps.Values(DefaultBuilderImages), image) {
				fmt.Fprintln(os.Stderr, "image referenced by tag which is discouraged: Tags are mutable and can point to a different artifact than the expected one")
			}
			img, err = remote.Image(ref)
			if err != nil {
				return "", fmt.Errorf("cannot get image from registry: %w", err)
			}
			cfg, err = img.ConfigFile()
			if err != nil {
				return "", fmt.Errorf("cannot get config for image: %w", err)
			}

			if cfg.Config.Labels != nil {
				if u, ok := cfg.Config.Labels["io.openshift.s2i.scripts-url"]; ok {
					return u, nil
				}
			}
		}
		return "", err
	}

	if img.Config != nil && img.Config.Labels != nil {
		if u, ok := img.Config.Labels["io.openshift.s2i.scripts-url"]; ok {
			return u, nil
		}
	}

	//nolint:staticcheck
	if img.ContainerConfig != nil && img.ContainerConfig.Labels != nil {
		if u, ok := img.ContainerConfig.Labels["io.openshift.s2i.scripts-url"]; ok {
			return u, nil
		}
	}

	return "", nil
}

// Builder Image chooses the correct builder image or defaults.
func BuilderImage(f fn.Function, builderName string) (string, error) {
	// delegate as the logic is shared amongst builders
	return builders.Image(f, builderName, DefaultBuilderImages)
}
