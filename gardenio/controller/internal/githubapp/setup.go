package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// VerifyLandsSetup is the check behind the Lands App's setup URL (Track B
// B-D3, step 7.6): a stranger proves they control a GitHub repository by
// installing the Lands App on it and authorizing it as themselves. It passes
// only when every one of these holds, in this order:
//
//  1. the client is the Lands App (a fleet credential never verifies a tenant);
//  2. the code exchanges for a user-to-server token;
//  3. that token's GitHub user is the applicant (their signed-in github_id);
//  4. the installation is one of the Lands App's own, and is not suspended;
//  5. it is installed on selected repositories, and the user can see exactly
//     one of them;
//  6. the user is an admin of that repository;
//  7. the repository's visibility is public, or private when AllowPrivate;
//  8. the repository is no larger than MaxRepoSizeKB, when set (Track B §5.4,
//     step 10.4: a land refuses a repository too big to clone into a pod).
//
// Any failure of 1 and 3-8, and a code GitHub refuses (2), is a *SetupRefusal
// naming the check. Anything else (GitHub unreachable, a 5xx) is a plain error:
// the applicant should try again, not be told no. The user token is revoked
// before returning either way.
func (c *Client) VerifyLandsSetup(ctx context.Context, check SetupCheck) (VerifiedRepo, error) {
	if c == nil || c.name != Lands {
		return VerifiedRepo{}, refuse(RefuseWrongApp, "only the Lands App verifies a Lands setup")
	}
	if check.InstallationID <= 0 {
		return VerifiedRepo{}, refuse(RefuseInstallation, "no installation id")
	}
	if check.ApplicantGitHubID <= 0 {
		return VerifiedRepo{}, refuse(RefuseWrongUser, "the applicant has no linked GitHub account")
	}

	tok, err := c.ExchangeUserCode(ctx, check.Code)
	if err != nil {
		var oe *OAuthError
		if errors.As(err, &oe) {
			return VerifiedRepo{}, refuse(RefuseCode, oe.Code)
		}
		return VerifiedRepo{}, err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = c.RevokeUserToken(rctx, tok)
	}()

	user, err := c.AuthenticatedUser(ctx, tok)
	if err != nil {
		return VerifiedRepo{}, err
	}
	if user.ID != check.ApplicantGitHubID {
		return VerifiedRepo{}, refuse(RefuseWrongUser, fmt.Sprintf("authorized as GitHub user %d (%s), signed in as %d", user.ID, user.Login, check.ApplicantGitHubID))
	}

	inst, err := c.Installation(ctx, check.InstallationID)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return VerifiedRepo{}, refuse(RefuseInstallation, "not an installation of the Lands App")
		}
		return VerifiedRepo{}, err
	}
	if inst.ID != check.InstallationID {
		return VerifiedRepo{}, refuse(RefuseInstallation, "GitHub returned a different installation")
	}
	if inst.SuspendedAt != nil {
		return VerifiedRepo{}, refuse(RefuseSuspended, "the installation is suspended")
	}
	if inst.RepositorySelection != "selected" {
		return VerifiedRepo{}, refuse(RefuseRepoCount, "installed on all repositories; choose only the one repository")
	}

	repos, total, err := c.UserInstallationRepos(ctx, tok, check.InstallationID)
	if err != nil {
		if isStatus(err, http.StatusNotFound) || isStatus(err, http.StatusForbidden) {
			return VerifiedRepo{}, refuse(RefuseInstallation, "the authorizing user cannot access this installation")
		}
		return VerifiedRepo{}, err
	}
	if total != 1 || len(repos) != 1 {
		return VerifiedRepo{}, refuse(RefuseRepoCount, fmt.Sprintf("the installation covers %d repositories you can see; a land needs exactly one", total))
	}
	repo := repos[0]
	owner, name, ok := strings.Cut(repo.FullName, "/")
	if !ok || owner == "" || name == "" || !strings.EqualFold(owner, inst.Account.Login) {
		return VerifiedRepo{}, refuse(RefuseInstallation, "the repository does not belong to the installation's account")
	}
	if !repo.Permissions.Admin {
		return VerifiedRepo{}, refuse(RefuseNotAdmin, repo.FullName)
	}
	visibility := strings.ToLower(repo.Visibility)
	if visibility == "" {
		// Older API shapes carry only `private`.
		visibility = "public"
		if repo.Private {
			visibility = "private"
		}
	}
	switch {
	case visibility == "public" && !repo.Private:
	case visibility == "private" && repo.Private && check.AllowPrivate:
	default:
		return VerifiedRepo{}, refuse(RefuseVisibility, visibility)
	}
	if check.MaxRepoSizeKB > 0 && repo.Size > check.MaxRepoSizeKB {
		return VerifiedRepo{}, refuse(RefuseRepoSize, fmt.Sprintf("%s is %d KB, over the %d KB limit", repo.FullName, repo.Size, check.MaxRepoSizeKB))
	}

	return VerifiedRepo{
		InstallationID: inst.ID,
		Repo:           RepoRef{Owner: owner, Name: name},
		RepoID:         repo.ID,
		Private:        repo.Private,
		Visibility:     visibility,
		SizeKB:         repo.Size,
		GitHubID:       user.ID,
		GitHubLogin:    user.Login,
		AccountCreated: user.CreatedAt,
		PublicRepos:    user.PublicRepos,
		Followers:      user.Followers,
	}, nil
}

// MaxRepoSizeEnv is the largest repository a land takes, in MB of GitHub's
// repository size (an abuse control, Track B §5.4 step 10.4): the setup check
// reads it in beads-viewer, and the controller again at every Lands token mint
// and refresh. Unset is MaxRepoSizeDefaultMB; 0 turns the check off.
const (
	MaxRepoSizeEnv       = "LANDS_MAX_REPO_SIZE_MB"
	MaxRepoSizeDefaultMB = 1024
)

// MaxRepoSizeMB reads MaxRepoSizeEnv; anything unparseable or negative is the
// default, so a typo never turns the check off.
func MaxRepoSizeMB(getenv func(string) string) int64 {
	v := strings.TrimSpace(getenv(MaxRepoSizeEnv))
	if v == "" {
		return MaxRepoSizeDefaultMB
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return MaxRepoSizeDefaultMB
	}
	return n
}

// SetupCheck is what the setup URL hands VerifyLandsSetup.
type SetupCheck struct {
	InstallationID    int64  // the callback's installation_id
	Code              string // the callback's one-time code; never logged
	ApplicantGitHubID int64  // the signed-in applicant's github_id claim
	AllowPrivate      bool   // private repositories may back a land (owner's OQ2 answer)
	MaxRepoSizeKB     int64  // the largest repository a land takes, in KB; 0 is no limit
}

// VerifiedRepo is a passed check: the repository the land will work on and the
// signals the operator reads at approval (§2.4).
type VerifiedRepo struct {
	InstallationID int64
	Repo           RepoRef
	RepoID         int64
	Private        bool
	Visibility     string
	SizeKB         int64 // GitHub's size of the repository, in kilobytes
	GitHubID       int64
	GitHubLogin    string
	AccountCreated time.Time
	PublicRepos    int
	Followers      int
}

// RefuseReason names the check a setup failed.
type RefuseReason string

const (
	RefuseWrongApp     RefuseReason = "wrong_app"
	RefuseCode         RefuseReason = "code"
	RefuseWrongUser    RefuseReason = "wrong_user"
	RefuseInstallation RefuseReason = "installation"
	RefuseSuspended    RefuseReason = "suspended"
	RefuseRepoCount    RefuseReason = "repo_count"
	RefuseNotAdmin     RefuseReason = "not_admin"
	RefuseVisibility   RefuseReason = "visibility"
	RefuseRepoSize     RefuseReason = "repo_size"
)

// SetupRefusal is a setup that failed a check. Detail is for the log and may
// name the GitHub account or repository; it never carries a credential.
type SetupRefusal struct {
	Reason RefuseReason
	Detail string
}

func (e *SetupRefusal) Error() string {
	return fmt.Sprintf("githubapp: lands setup refused (%s): %s", e.Reason, e.Detail)
}

func refuse(reason RefuseReason, detail string) error {
	return &SetupRefusal{Reason: reason, Detail: detail}
}

func isStatus(err error, status int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == status
}
