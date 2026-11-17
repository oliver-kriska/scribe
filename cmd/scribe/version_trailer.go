package main

import (
	"fmt"
	"sort"
	"strings"
)

// scribeVersionTrailerKey names the git trailer every scribe commit carries,
// so a team can tell which binary wrote each commit.
const scribeVersionTrailerKey = "Scribe-Version"

// teamVersionWindow is how far back doctor looks for each committer's version.
const teamVersionWindow = "14.days"

// devVersion is what a plain `go build` reports (no -ldflags stamp).
const devVersion = "dev"

// versionTrailer is passed as a second `git commit -m` paragraph. A message
// paragraph works on any git; `git commit --trailer` would need git >= 2.32
// and fail every commit on older machines.
func versionTrailer() string {
	return scribeVersionTrailerKey + ": " + version
}

// committerVersion is one author's newest Scribe-Version in the window.
// Version is "" when none of their recent commits carries the trailer.
type committerVersion struct {
	Email   string
	Version string
}

// latestVersionsByAuthor parses `git log --format=%ae%x00%(trailers:...)%x1e`
// output (newest first). Per email it keeps the newest commit WITH a trailer:
// a manual commit or a pull merge has none and must not hide the person's
// real scribe version. ok is false when git was too old to expand the
// placeholder (it then prints it literally).
func latestVersionsByAuthor(gitLog string) (versions []committerVersion, ok bool) {
	byEmail := map[string]string{}
	var order []string
	for record := range strings.SplitSeq(gitLog, "\x1e") {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		email, trailerValues, _ := strings.Cut(record, "\x00")
		email = strings.TrimSpace(email)
		value, _, _ := strings.Cut(strings.TrimSpace(trailerValues), "\n")
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "%(") {
			return nil, false
		}
		known, seen := byEmail[email]
		if !seen {
			order = append(order, email)
		}
		if known == "" {
			byEmail[email] = value
		}
	}
	for _, email := range order {
		versions = append(versions, committerVersion{Email: email, Version: byEmail[email]})
	}
	return versions, true
}

// sameVersion ignores a leading "v": GoReleaser stamps "0.6.3", while
// `make install` stamps the git tag "v0.6.3".
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// teamVersionCheck compares each committer's version with this binary's.
// selfEmail marks the user's own row: right after an upgrade it still shows
// the old version until this machine's next commit.
func teamVersionCheck(versions []committerVersion, binaryVersion, selfEmail string) check {
	var differing []string
	for _, cv := range versions {
		label := cv.Email
		if cv.Email == selfEmail {
			label += " (you)"
		}
		switch {
		case cv.Version == "":
			differing = append(differing, label+": pre-trailer")
		case sameVersion(cv.Version, binaryVersion):
		case cv.Version == devVersion:
			differing = append(differing, label+": dev build")
		default:
			differing = append(differing, label+": "+cv.Version)
		}
	}
	if len(differing) == 0 {
		return check{
			Section: "state", Name: "team-versions", Status: statusOK,
			Detail: fmt.Sprintf("%d recent committer(s) all on %s", len(versions), binaryVersion),
		}
	}
	sort.Strings(differing)
	return check{
		Section: "state", Name: "team-versions", Status: statusWarn,
		Detail: fmt.Sprintf("this binary is %s; differs: %s", binaryVersion, strings.Join(differing, ", ")),
		Fix:    "upgrade scribe on each listed machine; a (you) row clears after this machine's next commit",
	}
}

// checkTeamVersions reads recent commit trailers. It emits no row without
// git, without recent commits, or when git cannot report trailers.
func checkTeamVersions(root string) []check {
	if !hasGit(root) {
		return nil
	}
	gitLog, err := runCmdRaw(root, "git", "log", "--no-merges", "--since="+teamVersionWindow,
		"--format=%ae%x00%(trailers:key="+scribeVersionTrailerKey+",valueonly)%x1e")
	if err != nil {
		return nil
	}
	versions, ok := latestVersionsByAuthor(string(gitLog))
	if !ok || len(versions) == 0 {
		return nil
	}
	selfEmail := strings.TrimSpace(runCmd(root, "git", "config", "user.email"))
	return []check{teamVersionCheck(versions, version, selfEmail)}
}
