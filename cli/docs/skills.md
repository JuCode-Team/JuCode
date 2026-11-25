# Skills

JuCode loads skills from these sources:

1. installed user skills under `~/.jucode/skills`;
2. user-level skills under `~/.agents/skills` (the cross-tool convention directory);
3. project skills under `<project>/.jucode/skills` and `<project>/.agents/skills`, only after the project is trusted;
4. one optional extra GitHub source configured in `~/.jucode/config.json`.

Each skill is a directory containing `SKILL.md`. Frontmatter `name` and `description` fields
are used for discovery. A skill named `Code Review` is available as `/code-review`; text after
the slash command is passed to the skill as the user request. `/pin <skill>` keeps a skill's
instructions in the current session context.

Skill files usually live outside the workspace (`~/.jucode/skills`, `~/.agents/skills`), so the
read-only file tools (`read`, `ls`, `outline`, `ripgrep`) may read under each discovered skill's
directory in addition to the workspace. That lets the model open `SKILL.md` and follow relative
references inside the skill. Mutating tools stay confined to the workspace, and other outside
paths remain rejected.

## Lifecycle commands

```text
/skills list
/skills install <id>
/skills update <id>
/skills uninstall <id>
/skills enable <id>
/skills disable <id>
```

Install and update target the user skill directory and install from the extra GitHub source
below. Disable keeps the files installed but removes the skill from model context and
slash-command discovery. Project skills are source-controlled local resources, so lifecycle
commands do not modify them.

## Extra GitHub source

Set `extra_skills_source` to the built-in name `anthropic` or to an HTTPS GitHub repository:

```json
{
  "extra_skills_source": "anthropic"
}
```

The built-in source points to <https://github.com/anthropics/skills>. Its vendored index,
`crates/agent-core/src/anthropic-skills.json`, carries each skill's name, description, tags and
license and is pinned to a reviewed Git commit, so listing works without a GitHub request. A custom
repository is read through the public GitHub API at its default branch's current commit, and
installs come from that commit. Its skills may sit in `skills/<slug>/` or at the top level.
`/skills list` groups available skills by source.

`/skills install <slug>` or `/skills update <slug>` reads the repository tree from the GitHub API
and downloads the whole `skills/<slug>/` directory from `raw.githubusercontent.com`. Every path is
validated before it is joined to the install directory; links, submodules, absolute paths and
parent traversal are rejected. Downloads are limited to 20 MiB per file, 100 MiB in total and
4,096 files, and executable bits are kept. Files are staged next to the destination, and the old
install is replaced only after a `SKILL.md` exists.

Anthropic's `docx`, `pdf`, `pptx`, and `xlsx` skills are source-available rather than Apache-2.0
(`"redistributable": false` in the index). `/skills` lists them as not offered and will not
install them.

To refresh the built-in index, run `node scripts/refresh-anthropic-skills.mjs`. It pins the index
to the current `main` commit, reads each skill's `SKILL.md` frontmatter and `LICENSE.txt`, and
keeps the curated names and tags already in the index. Review the diff, especially the license
classification and the upstream changes since the previous commit, then run the agent-core tests.

## Desktop catalog

JuCode Desktop shows the Anthropic index in a catalog through the daemon's `skills_catalog` and
`skill_install` ops (see `docs/daemon-protocol.md`), which use the same install code as
`/skills`. The session's engine picks the directory: Claude Code sessions install into
`~/.claude/skills`, all others into `~/.jucode/skills`.

Other GitHub skill repositories join the catalog only when the daemon runs with
`JUCODE_SKILL_SOURCES="Name=https://github.com/owner/repo;…"`; none are listed by default. Their
skills may sit in `skills/<slug>/` or at the repository's top level. A repository is read at its
default branch's current commit (again after an hour; after five minutes when it could not be
read), and its skills install from that commit. A repository that cannot be read only adds a
warning. JuCode does not review these repositories.

A skill from a custom repository is offered only under an open-source license: the `license` in
its `SKILL.md` frontmatter, or else the repository's license as GitHub detects it. A skill
without one, or with terms such as "Complete terms in LICENSE.txt", is listed with its license
and repository link but not installed.

An installed source skill records its repository in `.jucode-source`. Installing a skill of the
same name from another repository is refused until the installed one is removed, and the catalog
marks a skill installed only for the repository it came from.

Desktop lists the source-available Anthropic document skills the same way: shown with their
license and repository link, not installed (the daemon's `skill_install` refuses them).

Skills are executable instructions and may include scripts. Treat installation like installing
software and review upstream content before using it with sensitive projects.
