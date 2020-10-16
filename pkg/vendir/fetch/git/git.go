package git

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	ctlconf "github.com/k14s/vendir/pkg/vendir/config"
	ctlfetch "github.com/k14s/vendir/pkg/vendir/fetch"
)

type Git struct {
	opts       ctlconf.DirectoryContentsGit
	infoLog    io.Writer
	refFetcher ctlfetch.RefFetcher
}

func NewGit(opts ctlconf.DirectoryContentsGit,
	infoLog io.Writer, refFetcher ctlfetch.RefFetcher) *Git {

	return &Git{opts, infoLog, refFetcher}
}

type GitInfo struct {
	SHA         string
	Tags        []string
	CommitTitle string
}

func (t *Git) Retrieve(dstPath string, tempArea ctlfetch.TempArea) (GitInfo, error) {
	if len(t.opts.URL) == 0 {
		return GitInfo{}, fmt.Errorf("Expected non-empty URL")
	}
	if len(t.opts.Ref) == 0 {
		return GitInfo{}, fmt.Errorf("Expected non-empty ref (could be branch, tag, commit)")
	}

	err := t.fetch(dstPath, false, tempArea)
	if err != nil {
		return GitInfo{}, fmt.Errorf("Cloning: %s", err)
	}

	info := GitInfo{}

	out, _, err := t.run([]string{"rev-parse", "HEAD"}, nil, dstPath)
	if err != nil {
		return GitInfo{}, err
	}

	info.SHA = strings.TrimSpace(out)

	out, _, err = t.run([]string{"describe", "--tags", info.SHA}, nil, dstPath)
	if err == nil {
		info.Tags = strings.Split(strings.TrimSpace(out), "\n")
	}

	out, _, err = t.run([]string{"log", "-n", "1", "--pretty=%B", info.SHA}, nil, dstPath)
	if err != nil {
		return GitInfo{}, err
	}

	info.CommitTitle = strings.TrimSpace(out)

	return info, nil
}

func (t *Git) Tags(dstPath string, tempArea ctlfetch.TempArea) ([]string, error) {
	if len(t.opts.URL) == 0 {
		return nil, fmt.Errorf("Expected non-empty URL")
	}
	// Not caring about any ref at this point

	err := t.fetch(dstPath, true, tempArea)
	if err != nil {
		return nil, fmt.Errorf("Cloning: %s", err)
	}

	out, _, err := t.run([]string{"tag", "-l"}, nil, dstPath)
	if err != nil {
		return nil, err
	}

	return strings.Split(out, "\n"), nil
}

func (t *Git) fetch(dstPath string, skeleton bool, tempArea ctlfetch.TempArea) error {
	authOpts, err := t.getAuthOpts()
	if err != nil {
		return err
	}

	authDir, err := tempArea.NewTempDir("git-auth")
	if err != nil {
		return err
	}

	defer os.RemoveAll(authDir)

	env := os.Environ()

	if authOpts.IsPresent() {
		sshCmd := []string{"ssh", "-o", "ServerAliveInterval=30", "-o", "ForwardAgent=no", "-F", "/dev/null"}

		if authOpts.PrivateKey != nil {
			path := filepath.Join(authDir, "private-key")

			err = ioutil.WriteFile(path, []byte(*authOpts.PrivateKey), 0600)
			if err != nil {
				return fmt.Errorf("Writing private key: %s", err)
			}

			sshCmd = append(sshCmd, "-i", path, "-o", "IdentitiesOnly=yes")
		}

		if authOpts.KnownHosts != nil {
			path := filepath.Join(authDir, "known-hosts")

			err = ioutil.WriteFile(path, []byte(*authOpts.KnownHosts), 0600)
			if err != nil {
				return fmt.Errorf("Writing known hosts: %s", err)
			}

			sshCmd = append(sshCmd, "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+path)
		} else {
			sshCmd = append(sshCmd, "-o", "StrictHostKeyChecking=no")
		}

		env = append(env, "GIT_SSH_COMMAND="+strings.Join(sshCmd, " "))
	}

	if t.opts.LFSSkipSmudge {
		env = append(env, "GIT_LFS_SKIP_SMUDGE=1")
	}

	gitUrl := t.opts.URL
	gitCredsPath := filepath.Join(authDir, ".git-credentials")

	if authOpts.Username != nil && authOpts.Password != nil {
		if !strings.HasPrefix(gitUrl, "https://") {
			return fmt.Errorf("Username/password authentication is only supported for https remotes")
		}

		gitCredsUrl, err := url.Parse(gitUrl)
		if err != nil {
			return fmt.Errorf("Parsing git remote url: %s", err)
		}

		gitCredsUrl.User = url.UserPassword(*authOpts.Username, *authOpts.Password)
		gitCredsUrl.Path = ""

		err = ioutil.WriteFile(gitCredsPath, []byte(gitCredsUrl.String()+"\n"), 0600)
		if err != nil {
			return fmt.Errorf("Writing %s: %s", gitCredsPath, err)
		}
	}

	argss := [][]string{
		{"init"},
		{"config", "credential.helper", "store --file " + gitCredsPath},
		{"remote", "add", "origin", gitUrl},
		{"fetch", "origin"},
	}

	if !skeleton {
		argss = append(argss, [][]string{
			// TODO following causes rev-parse HEAD to fail:
			// {"checkout", t.opts.Ref, "--recurse-submodules", "."},
			{"-c", "advice.detachedHead=false", "checkout", t.opts.Ref},
			{"submodule", "update", "--init", "--recursive"},
			// TODO shallow clones?
		}...)
	}

	for _, args := range argss {
		_, _, err := t.run(args, env, dstPath)
		if err != nil {
			return err
		}
	}

	return nil
}

func (t *Git) run(args []string, env []string, dstPath string) (string, string, error) {
	var stdoutBs, stderrBs bytes.Buffer

	cmd := exec.Command("git", args...)
	cmd.Env = env
	cmd.Dir = dstPath
	cmd.Stdout = io.MultiWriter(t.infoLog, &stdoutBs)
	cmd.Stderr = io.MultiWriter(t.infoLog, &stderrBs)

	t.infoLog.Write([]byte(fmt.Sprintf("--> git %s\n", strings.Join(args, " "))))

	err := cmd.Run()
	if err != nil {
		return "", "", fmt.Errorf("Git %s: %s (stderr: %s)", args, err, stderrBs.String())
	}

	return stdoutBs.String(), stderrBs.String(), nil
}

type gitAuthOpts struct {
	PrivateKey *string
	KnownHosts *string
	Username   *string
	Password   *string
}

func (o gitAuthOpts) IsPresent() bool {
	return o.PrivateKey != nil || o.KnownHosts != nil || o.Username != nil || o.Password != nil
}

func (t *Git) getAuthOpts() (gitAuthOpts, error) {
	var opts gitAuthOpts

	if t.opts.SecretRef != nil {
		secret, err := t.refFetcher.GetSecret(t.opts.SecretRef.Name)
		if err != nil {
			return opts, err
		}

		for name, val := range secret.Data {
			switch name {
			case ctlconf.SecretK8sCoreV1SSHAuthPrivateKey:
				key := string(val)
				opts.PrivateKey = &key
			case ctlconf.SecretSSHAuthKnownHosts:
				hosts := string(val)
				opts.KnownHosts = &hosts
			case ctlconf.SecretK8sCorev1BasicAuthUsernameKey:
				username := string(val)
				opts.Username = &username
			case ctlconf.SecretK8sCorev1BasicAuthPasswordKey:
				password := string(val)
				opts.Password = &password
			default:
				return opts, fmt.Errorf("Unknown secret field '%s' in secret '%s'", name, t.opts.SecretRef.Name)
			}
		}
	}

	return opts, nil
}
