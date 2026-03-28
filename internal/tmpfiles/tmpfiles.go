package tmpfiles

import (
	"bufio"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

var searchPaths = []string{
	"/etc/tmpfiles.d",
	"/run/tmpfiles.d",
	"/usr/lib/tmpfiles.d",
	"/lib/tmpfiles.d",
}

func ApplyRuntimeDirs() error {
	for _, dir := range searchPaths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
				continue
			}
			if err := applyFile(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func applyFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if fields[0] != "d" && fields[0] != "D" {
			continue
		}
		target := fields[1]
		if !strings.HasPrefix(target, "/run/") && !strings.HasPrefix(target, "/var/run/") {
			continue
		}

		mode := os.FileMode(0o755)
		if fields[2] != "-" {
			parsed, err := strconv.ParseUint(fields[2], 8, 32)
			if err != nil {
				return fmt.Errorf("parse tmpfiles mode in %s: %w", path, err)
			}
			mode = os.FileMode(parsed)
		}

		if err := os.MkdirAll(target, mode); err != nil {
			return err
		}
		if err := os.Chmod(target, mode); err != nil {
			return err
		}

		uid, gid, ok, err := lookupOwnership(fields[3], fields[4])
		if err != nil {
			return err
		}
		if ok {
			if err := os.Chown(target, uid, gid); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func lookupOwnership(userName, groupName string) (int, int, bool, error) {
	if userName == "-" && groupName == "-" {
		return 0, 0, false, nil
	}

	uid := os.Getuid()
	gid := os.Getgid()

	if userName != "-" {
		value, resolvedGID, err := lookupUser(userName)
		if err != nil {
			return 0, 0, false, err
		}
		uid = value
		if groupName == "-" {
			gid = resolvedGID
		}
	}

	if groupName != "-" {
		value, err := lookupGroup(groupName)
		if err != nil {
			return 0, 0, false, err
		}
		gid = value
	}

	return uid, gid, true, nil
}

func lookupUser(name string) (int, int, error) {
	if uid, err := strconv.Atoi(name); err == nil {
		return uid, os.Getgid(), nil
	}
	info, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(info.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(info.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func lookupGroup(name string) (int, error) {
	if gid, err := strconv.Atoi(name); err == nil {
		return gid, nil
	}
	info, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(info.Gid)
}
