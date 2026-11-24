package supervisor

import (
	"errors"
	"io"
	"os"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

var (
	gitBranch   = regexp.MustCompile(`^[\x21-\x7e]{1,255}$`)
	detachedSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// Branch reads HEAD relative to the pinned checkout. "none" means there is no
// .git entry, not that HEAD was unreadable. The bootstrap uses the same check.
func (d *Directory) Branch() (string, error) { return branchAt(int(d.file.Fd())) }

func branchAt(fd int) (string, error) {
	git, err := openGitAt(fd, ".git", 0)
	if errors.Is(err, unix.ENOENT) {
		return "none", nil
	}
	if err != nil {
		return "", ErrCheckoutChanged
	}
	defer git.Close()
	info, err := git.Stat()
	if err != nil {
		return "", ErrCheckoutChanged
	}
	if !info.IsDir() {
		line := gitLine(git)
		path, found := strings.CutPrefix(line, "gitdir: ")
		path = strings.TrimSpace(path)
		if !found || path == "" {
			return "", ErrCheckoutChanged
		}
		// Git worktrees use either an absolute or checkout-relative pointer.
		git, err = openGitAt(fd, path, unix.O_DIRECTORY)
		if err != nil {
			return "", ErrCheckoutChanged
		}
		defer git.Close()
	}
	head, err := openGitAt(int(git.Fd()), "HEAD", 0)
	if err != nil {
		return "", ErrCheckoutChanged
	}
	defer head.Close()
	line := gitLine(head)
	if name, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok && gitBranch.MatchString(name) {
		return name, nil
	}
	if detachedSHA.MatchString(line) {
		return "detached:" + line, nil
	}
	return "", ErrCheckoutChanged
}

func openGitAt(dir int, path string, extra int) (*os.File, error) {
	fd, err := unix.Openat(dir, path, extra|unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "checkout metadata"), nil
}

func gitLine(f *os.File) string {
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return ""
	}
	return strings.TrimSpace(string(b))
}
