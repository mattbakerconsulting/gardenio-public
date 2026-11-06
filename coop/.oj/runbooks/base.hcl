# Shared libraries: GitHub issue tracking pipeline.

import "oj/github" {
  alias = "github"

  const "check" { value = "make check" }
}

# Disabled: worker names conflict with oddjobs project.
# Re-enable once oj supports project-scoped worker names.

# import "oj/wok" {
#   alias = "wok"
#
#   const "prefix" { value = "coop" }
#   const "check"  { value = "make check" }
#   const "submit" { value = "oj queue push merges --var branch=\"$branch\" --var title=\"$title\"" }
# }

# import "oj/git" {
#   alias = "git"
#
#   const "check" { value = "make check" }
# }
