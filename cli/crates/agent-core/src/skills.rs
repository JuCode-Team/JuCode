use serde_json::Value;
use std::{
    collections::BTreeSet,
    fs,
    io::{self, Read},
    path::{Component, Path, PathBuf},
    time::{Duration, SystemTime, UNIX_EPOCH},
};

/// Limit for one download: a package, a single skill file.
const MAX_PACKAGE_BYTES: usize = 20 * 1024 * 1024;
const MAX_TREE_BYTES: usize = 4 * 1024 * 1024;
const MAX_EXTRACTED_BYTES: u64 = 100 * 1024 * 1024;
const MAX_PACKAGE_FILES: usize = 4096;
const SKILL_STATE_FILE: &str = "skills-state.json";
/// In a skill installed from a GitHub source: the repository it came from.
const SOURCE_FILE: &str = ".jucode-source";
pub const ANTHROPIC_SKILLS_URL: &str = "https://github.com/anthropics/skills";
const ANTHROPIC_SKILLS_INDEX: &str = include_str!("anthropic-skills.json");

/// A skill in a GitHub repository's `<dir>/<id>/` directory.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SourceSkill {
    pub id: String,
    pub name: String,
    pub description: String,
    pub tags: Vec<String>,
    pub license: String,
    /// False for source-available skills whose terms forbid redistribution.
    pub redistributable: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SkillSource {
    pub name: String,
    pub repository: String,
    /// A commit SHA; empty follows the default branch.
    pub revision: String,
    /// The directory holding the skill folders: `skills`, or empty for the
    /// repository's top level.
    pub dir: String,
    pub skills: Vec<SourceSkill>,
}

impl SkillSource {
    pub fn homepage(&self, id: &str) -> String {
        format!(
            "{}/tree/{}/{}",
            self.repository,
            self.git_ref(),
            self.skill_path(id)
        )
    }

    /// `<dir>/<id>` in the repository.
    fn skill_path(&self, id: &str) -> String {
        if self.dir.is_empty() {
            id.to_string()
        } else {
            format!("{}/{id}", self.dir)
        }
    }

    fn git_ref(&self) -> &str {
        if self.revision.is_empty() {
            "HEAD"
        } else {
            &self.revision
        }
    }
}

/// Where JuCode loads installed user skills from: `~/.jucode/skills`.
pub fn profile_skills_dir() -> io::Result<PathBuf> {
    Ok(crate::config::profile_dir()?.join("skills"))
}

/// The bundled index of github.com/anthropics/skills, pinned to a reviewed
/// commit so listing needs no GitHub request.
pub fn anthropic_source() -> Result<SkillSource, String> {
    let value = serde_json::from_str(ANTHROPIC_SKILLS_INDEX)
        .map_err(|error| format!("invalid bundled Anthropic skills index: {error}"))?;
    parse_source_index(&value)
}

pub fn fetch_extra_skill_source(spec: &str) -> Result<Option<SkillSource>, String> {
    let spec = spec.trim();
    if spec.is_empty() {
        return Ok(None);
    }
    if spec == "anthropic" || normalize_repository_url(spec) == ANTHROPIC_SKILLS_URL {
        return anthropic_source().map(Some);
    }

    let (_, repository) = github_repository_parts(spec)?;
    fetch_github_source(&repository, spec).map(Some)
}

/// A GitHub repository's skills, wherever its `SKILL.md` files sit: every
/// `<id>/SKILL.md` at the top level or `<dir>/<id>/SKILL.md` one level down,
/// from the directory holding the most. Read at the default branch's current
/// commit, which installs then use, so what is installed is what was listed.
/// Names, descriptions and licenses come from each SKILL.md's frontmatter
/// (read in parallel); a skill without a license has the repository's.
pub fn fetch_github_source(name: &str, repository: &str) -> Result<SkillSource, String> {
    read_github_source(name, repository, &download)
}

fn read_github_source(
    name: &str,
    repository: &str,
    fetch: &(dyn Fn(&str, usize) -> io::Result<Vec<u8>> + Sync),
) -> Result<SkillSource, String> {
    let (owner, repo) = github_repository_parts(repository)?;
    let json = |url: &str| -> Result<Value, String> {
        let bytes = fetch(url, MAX_TREE_BYTES).map_err(|error| error.to_string())?;
        serde_json::from_slice(&bytes).map_err(|error| error.to_string())
    };
    let api = format!("https://api.github.com/repos/{owner}/{repo}");
    let meta = json(&api)?;
    let branch = meta["default_branch"]
        .as_str()
        .ok_or_else(|| format!("{repository} has no default branch"))?;
    let revision = json(&format!("{api}/branches/{}", encode_url_path(branch)))?["commit"]["sha"]
        .as_str()
        .map(str::to_string)
        .ok_or_else(|| format!("{repository}: no commit for {branch}"))?;
    validate_revision(&revision).map_err(|error| error.to_string())?;
    let repository_license = meta["license"]["spdx_id"]
        .as_str()
        .filter(|id| *id != "NOASSERTION")
        .unwrap_or_default()
        .to_string();
    let tree = json(&format!("{api}/git/trees/{revision}?recursive=1"))?;
    let mut by_dir: std::collections::BTreeMap<String, Vec<String>> = Default::default();
    for path in tree["tree"]
        .as_array()
        .into_iter()
        .flatten()
        .filter(|entry| entry["type"] == "blob")
        .filter_map(|entry| entry["path"].as_str())
    {
        let parts: Vec<&str> = path.split('/').collect();
        let (dir, id) = match parts.as_slice() {
            [id, "SKILL.md"] => ("", *id),
            [dir, id, "SKILL.md"] => (*dir, *id),
            _ => continue,
        };
        if !id.starts_with('.') && validate_skill_id(id).is_ok() {
            by_dir
                .entry(dir.to_string())
                .or_default()
                .push(id.to_string());
        }
    }
    let (dir, ids) = by_dir
        .into_iter()
        // `skills/` or the top level hold a repository's own skills; another
        // folder (a vendored collection) only when there are none there.
        .max_by_key(|(dir, ids)| (dir == "skills" || dir.is_empty(), ids.len()))
        .ok_or_else(|| format!("no SKILL.md found in {repository}"))?;
    let read = |id: &str| -> [String; 3] {
        let path = if dir.is_empty() {
            format!("{id}/SKILL.md")
        } else {
            format!("{dir}/{id}/SKILL.md")
        };
        let text = fetch(
            &format!(
                "https://raw.githubusercontent.com/{owner}/{repo}/{revision}/{}",
                encode_url_path(&path)
            ),
            256 * 1024,
        )
        .map(|bytes| String::from_utf8_lossy(&bytes).into_owned())
        .unwrap_or_default();
        ["name", "description", "license"]
            .map(|key| crate::prompt::read_frontmatter_field(&text, key).unwrap_or_default())
    };
    let metas: Vec<[String; 3]> = std::thread::scope(|scope| {
        let read = &read;
        let handles: Vec<_> = ids.iter().map(|id| scope.spawn(move || read(id))).collect();
        handles
            .into_iter()
            .map(|handle| handle.join().unwrap_or_default())
            .collect()
    });
    let mut skills: Vec<SourceSkill> = ids
        .iter()
        .zip(metas)
        .map(|(id, [skill_name, description, license])| {
            let license = if license.trim().is_empty() {
                repository_license.clone()
            } else {
                license.trim().to_string()
            };
            SourceSkill {
                id: id.clone(),
                name: if skill_name.is_empty() {
                    id.clone()
                } else {
                    skill_name
                },
                description,
                tags: Vec::new(),
                redistributable: open_license(&license),
                license,
            }
        })
        .collect();
    skills.sort_by(|left, right| left.id.cmp(&right.id));
    Ok(SkillSource {
        name: name.to_string(),
        repository: normalize_repository_url(repository),
        revision,
        dir,
        skills,
    })
}

/// Whether `license` (an SPDX id, or a frontmatter line such as "MIT
/// License") is an open-source license that lets JuCode offer the skill.
/// Anything else, a missing license included, is not offered.
fn open_license(license: &str) -> bool {
    const OPEN: [&str; 16] = [
        "mit",
        "apache-2.0",
        "bsd-2-clause",
        "bsd-3-clause",
        "isc",
        "0bsd",
        "zlib",
        "unlicense",
        "cc0-1.0",
        "cc-by-4.0",
        "mpl-2.0",
        "gpl-2.0",
        "gpl-3.0",
        "lgpl-2.1",
        "lgpl-3.0",
        "agpl-3.0",
    ];
    let text = license.trim().to_ascii_lowercase();
    let text = text.strip_suffix(" license").unwrap_or(&text);
    let id = match text {
        "apache license 2.0" | "apache license, version 2.0" | "apache 2.0" => "apache-2.0",
        other => other,
    };
    OPEN.contains(&id)
}

fn parse_source_index(value: &Value) -> Result<SkillSource, String> {
    let name = read_string(value, "name").ok_or_else(|| "source index missing name".to_string())?;
    let repository = read_string(value, "repository")
        .ok_or_else(|| "source index missing repository".to_string())?;
    github_repository_parts(&repository)?;
    let revision = read_string(value, "revision").unwrap_or_default();
    if !revision.is_empty() {
        validate_revision(&revision).map_err(|error| error.to_string())?;
    }
    let skill_values = value
        .get("skills")
        .and_then(Value::as_array)
        .ok_or_else(|| "source index missing skills".to_string())?;
    let mut skills: Vec<SourceSkill> = Vec::with_capacity(skill_values.len());
    for item in skill_values {
        let id = read_string(item, "id").ok_or_else(|| "source skill missing id".to_string())?;
        validate_skill_id(&id).map_err(|error| error.to_string())?;
        let field = |key: &str| {
            read_string(item, key).ok_or_else(|| format!("source skill {id} missing {key}"))
        };
        let skill = SourceSkill {
            name: field("name")?,
            description: field("description")?,
            tags: read_strings(item, "tags"),
            license: field("license")?,
            redistributable: item
                .get("redistributable")
                .and_then(Value::as_bool)
                .ok_or_else(|| format!("source skill {id} missing redistributable"))?,
            id,
        };
        if skills.iter().any(|existing| existing.id == skill.id) {
            return Err(format!("duplicate source skill id: {}", skill.id));
        }
        skills.push(skill);
    }
    Ok(SkillSource {
        name,
        repository: normalize_repository_url(&repository),
        revision,
        dir: "skills".to_string(),
        skills,
    })
}

/// Installs the whole `skills/<id>/` directory of `source` into
/// `skills_dir/<id>`, replacing an earlier install only once every file has
/// downloaded.
pub fn install_source_skill(
    skills_dir: &Path,
    source: &SkillSource,
    skill: &SourceSkill,
) -> io::Result<PathBuf> {
    install_github_skill(skills_dir, source, skill, &download)
}

fn install_github_skill(
    skills_dir: &Path,
    source: &SkillSource,
    skill: &SourceSkill,
    fetch: &dyn Fn(&str, usize) -> io::Result<Vec<u8>>,
) -> io::Result<PathBuf> {
    validate_skill_id(&skill.id)?;
    // Two sources can name a skill alike; one must not replace the other's.
    if let Some(other) =
        installed_source(skills_dir, &skill.id).filter(|other| *other != source.repository)
    {
        return Err(io::Error::new(
            io::ErrorKind::AlreadyExists,
            format!(
                "skill {} from {other} is already installed; remove it first",
                skill.id
            ),
        ));
    }
    let (owner, repository) = github_repository_parts(&source.repository)
        .map_err(|error| io::Error::new(io::ErrorKind::InvalidInput, error))?;
    if !source.revision.is_empty() {
        validate_revision(&source.revision)?;
    }
    let git_ref = source.git_ref();
    let tree = fetch(
        &format!(
            "https://api.github.com/repos/{owner}/{repository}/git/trees/{git_ref}?recursive=1"
        ),
        MAX_TREE_BYTES,
    )?;
    let tree = serde_json::from_slice::<Value>(&tree)
        .map_err(|error| io::Error::new(io::ErrorKind::InvalidData, error))?;
    if tree.get("truncated").and_then(Value::as_bool) == Some(true) {
        return Err(invalid_data(format!(
            "GitHub returned a truncated tree for {}",
            source.repository
        )));
    }

    let prefix = format!("{}/", source.skill_path(&skill.id));
    let mut files = Vec::new();
    let mut declared_bytes = 0_u64;
    for entry in tree
        .get("tree")
        .and_then(Value::as_array)
        .ok_or_else(|| invalid_data("GitHub tree response missing tree".to_string()))?
    {
        let path = entry
            .get("path")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let Some(relative) = path.strip_prefix(&prefix) else {
            continue;
        };
        let relative = safe_path_components(Path::new(relative))
            .ok_or_else(|| invalid_data(format!("unsafe path in GitHub tree: {path}")))?;
        let mode = entry
            .get("mode")
            .and_then(Value::as_str)
            .unwrap_or_default();
        match entry.get("type").and_then(Value::as_str) {
            Some("tree") => continue,
            Some("blob") if mode == "100644" || mode == "100755" => {}
            _ => {
                return Err(invalid_data(format!(
                    "unsupported GitHub tree entry: {path}"
                )))
            }
        }
        let size = entry
            .get("size")
            .and_then(Value::as_u64)
            .ok_or_else(|| invalid_data(format!("GitHub tree entry has no size: {path}")))?;
        if size > MAX_PACKAGE_BYTES as u64 {
            return Err(invalid_data(format!(
                "skill file exceeds {MAX_PACKAGE_BYTES} byte limit: {path}"
            )));
        }
        declared_bytes = declared_bytes.saturating_add(size);
        if declared_bytes > MAX_EXTRACTED_BYTES {
            return Err(invalid_data(format!(
                "skill exceeds {MAX_EXTRACTED_BYTES} byte limit"
            )));
        }
        files.push((path.to_string(), relative, mode == "100755"));
        if files.len() > MAX_PACKAGE_FILES {
            return Err(invalid_data(format!(
                "skill exceeds {MAX_PACKAGE_FILES} file limit"
            )));
        }
    }
    if files.is_empty() {
        return Err(io::Error::new(
            io::ErrorKind::NotFound,
            format!(
                "skill files not found in {}: {}",
                source.repository, skill.id
            ),
        ));
    }

    let dir = skills_dir.join(&skill.id);
    let staging = staging_dir(&dir, "download");
    recreate_dir(&staging)?;
    let downloaded = (|| {
        let mut actual_bytes = 0_u64;
        for (path, relative, executable) in files {
            let url = format!(
                "https://raw.githubusercontent.com/{owner}/{repository}/{git_ref}/{}",
                encode_url_path(&path)
            );
            let bytes = fetch(&url, MAX_PACKAGE_BYTES)?;
            actual_bytes = actual_bytes.saturating_add(bytes.len() as u64);
            if actual_bytes > MAX_EXTRACTED_BYTES {
                return Err(invalid_data(format!(
                    "skill exceeds {MAX_EXTRACTED_BYTES} byte limit"
                )));
            }
            let out = staging.join(relative);
            if let Some(parent) = out.parent() {
                fs::create_dir_all(parent)?;
            }
            fs::write(&out, bytes)?;
            set_mode(&out, Some(if executable { 0o755 } else { 0o644 }))?;
        }
        if !staging.join("SKILL.md").is_file() {
            return Err(invalid_data(format!(
                "skill {} does not contain SKILL.md",
                skill.id
            )));
        }
        fs::write(staging.join(SOURCE_FILE), &source.repository)
    })();
    if let Err(error) = downloaded {
        let _ = fs::remove_dir_all(&staging);
        return Err(error);
    }
    atomic_replace_dir(&staging, &dir)?;
    Ok(dir)
}

pub fn uninstall_skill(profile_dir: &Path, id: &str) -> io::Result<bool> {
    let skills_dir = profile_dir.join("skills");
    let target = skills_dir.join(safe_skill_dir(id));
    if !target.exists() {
        return Ok(false);
    }
    ensure_direct_child(&skills_dir, &target)?;
    fs::remove_dir_all(&target)?;
    set_skill_enabled(profile_dir, id, true)?;
    Ok(true)
}

pub fn set_skill_enabled(profile_dir: &Path, id: &str, enabled: bool) -> io::Result<()> {
    let id = safe_skill_dir(id);
    let mut disabled = read_disabled_skills(profile_dir)?;
    if enabled {
        disabled.remove(&id);
    } else {
        disabled.insert(id);
    }
    write_disabled_skills(profile_dir, &disabled)
}

pub fn is_skill_path_enabled(profile_dir: &Path, path: &Path) -> io::Result<bool> {
    let root = profile_dir.join("skills");
    let Ok(relative) = path.strip_prefix(&root) else {
        return Ok(true);
    };
    let Some(Component::Normal(id)) = relative.components().next() else {
        return Ok(true);
    };
    let Some(id) = id.to_str() else {
        return Ok(false);
    };
    Ok(!read_disabled_skills(profile_dir)?.contains(id))
}

pub fn installed_skill_ids(profile_dir: &Path) -> io::Result<Vec<String>> {
    let root = profile_dir.join("skills");
    if !root.exists() {
        return Ok(Vec::new());
    }
    let disabled = read_disabled_skills(profile_dir)?;
    let mut installed = Vec::new();
    for entry in fs::read_dir(root)? {
        let entry = entry?;
        if entry.path().join("SKILL.md").exists() {
            let id = entry.file_name().to_string_lossy().to_string();
            installed.push(if disabled.contains(&id) {
                format!("{id} (disabled)")
            } else {
                id
            });
        }
    }
    installed.sort();
    Ok(installed)
}

/// The repository an installed skill came from; None for one installed
/// before sources were recorded.
pub fn installed_source(skills_dir: &Path, id: &str) -> Option<String> {
    fs::read_to_string(skills_dir.join(safe_skill_dir(id)).join(SOURCE_FILE))
        .ok()
        .map(|text| text.trim().to_string())
        .filter(|text| !text.is_empty())
}

pub fn skill_installed(skills_dir: &Path, id: &str) -> bool {
    skills_dir
        .join(safe_skill_dir(id))
        .join("SKILL.md")
        .is_file()
}

fn read_string(value: &Value, key: &str) -> Option<String> {
    value
        .get(key)
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(str::to_string)
}

fn read_strings(value: &Value, key: &str) -> Vec<String> {
    value
        .get(key)
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(Value::as_str)
        .map(str::trim)
        .filter(|item| !item.is_empty())
        .map(str::to_string)
        .collect()
}

fn download(url: &str, limit: usize) -> io::Result<Vec<u8>> {
    if let Some(path) = url.strip_prefix("file://") {
        return read_bounded(fs::File::open(path)?, limit);
    }
    if !url.contains("://") {
        return read_bounded(fs::File::open(url)?, limit);
    }
    let response = ureq::get(url)
        .set("User-Agent", "jucode")
        .timeout(Duration::from_secs(60))
        .call()
        .map_err(|error| io::Error::other(error.to_string()))?;
    read_bounded(response.into_reader(), limit)
}

fn read_bounded(reader: impl Read, limit: usize) -> io::Result<Vec<u8>> {
    let mut reader = reader.take((limit + 1) as u64);
    let mut bytes = Vec::new();
    reader.read_to_end(&mut bytes)?;
    if bytes.len() > limit {
        return Err(invalid_data(format!("download exceeds {limit} byte limit")));
    }
    Ok(bytes)
}

fn invalid_data(message: String) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}

#[cfg(unix)]
fn set_mode(path: &Path, mode: Option<u32>) -> io::Result<()> {
    use std::os::unix::fs::PermissionsExt;

    if let Some(mode) = mode {
        fs::set_permissions(path, fs::Permissions::from_mode(mode))?;
    }
    Ok(())
}

#[cfg(not(unix))]
fn set_mode(_path: &Path, _mode: Option<u32>) -> io::Result<()> {
    Ok(())
}

fn safe_path_components(path: &Path) -> Option<PathBuf> {
    let mut out = PathBuf::new();
    for component in path.components() {
        match component {
            Component::Normal(part) => out.push(part),
            Component::CurDir => {}
            _ => return None,
        }
    }
    if out.as_os_str().is_empty() {
        None
    } else {
        Some(out)
    }
}

fn recreate_dir(dir: &Path) -> io::Result<()> {
    if dir.exists() {
        fs::remove_dir_all(dir)?;
    }
    fs::create_dir_all(dir)
}

fn staging_dir(dir: &Path, label: &str) -> PathBuf {
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_nanos())
        .unwrap_or(0);
    let name = dir
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or("skill");
    dir.with_file_name(format!(".{name}-{label}-{nonce}"))
}

fn atomic_replace_dir(staging: &Path, destination: &Path) -> io::Result<()> {
    let parent = destination
        .parent()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "skill has no parent"))?;
    fs::create_dir_all(parent)?;
    let backup = staging_dir(destination, "backup");
    let had_destination = destination.exists();
    if had_destination {
        fs::rename(destination, &backup)?;
    }
    if let Err(error) = fs::rename(staging, destination) {
        if had_destination {
            let _ = fs::rename(&backup, destination);
        }
        let _ = fs::remove_dir_all(staging);
        return Err(error);
    }
    if had_destination {
        let _ = fs::remove_dir_all(backup);
    }
    Ok(())
}

fn ensure_direct_child(parent: &Path, child: &Path) -> io::Result<()> {
    if child.parent() == Some(parent) {
        Ok(())
    } else {
        Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "skill path escapes skills directory",
        ))
    }
}

fn read_disabled_skills(profile_dir: &Path) -> io::Result<BTreeSet<String>> {
    let path = profile_dir.join(SKILL_STATE_FILE);
    if !path.exists() {
        return Ok(BTreeSet::new());
    }
    let content = fs::read_to_string(path)?;
    let value = serde_json::from_str::<Value>(&content).unwrap_or(Value::Null);
    Ok(value
        .get("disabled")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(Value::as_str)
        .map(str::to_string)
        .collect())
}

fn write_disabled_skills(profile_dir: &Path, disabled: &BTreeSet<String>) -> io::Result<()> {
    fs::create_dir_all(profile_dir)?;
    let path = profile_dir.join(SKILL_STATE_FILE);
    let temp = profile_dir.join(format!(".{SKILL_STATE_FILE}.tmp"));
    let value = serde_json::json!({ "disabled": disabled });
    fs::write(
        &temp,
        format!("{}\n", serde_json::to_string_pretty(&value)?),
    )?;
    match fs::rename(&temp, &path) {
        Ok(()) => Ok(()),
        Err(error) if path.exists() => {
            let backup = profile_dir.join(format!(".{SKILL_STATE_FILE}.backup"));
            let _ = fs::remove_file(&backup);
            fs::rename(&path, &backup)?;
            if let Err(move_error) = fs::rename(&temp, &path) {
                let _ = fs::rename(&backup, &path);
                let _ = fs::remove_file(&temp);
                return Err(io::Error::new(
                    move_error.kind(),
                    format!("{error}; replacement failed: {move_error}"),
                ));
            }
            let _ = fs::remove_file(backup);
            Ok(())
        }
        Err(error) => {
            let _ = fs::remove_file(temp);
            Err(error)
        }
    }
}

fn validate_skill_id(id: &str) -> io::Result<()> {
    if id.is_empty()
        || id.starts_with('-')
        || id.ends_with('-')
        || !id
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-')
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            format!("unsafe source skill id: {id}"),
        ));
    }
    Ok(())
}

fn validate_revision(revision: &str) -> io::Result<&str> {
    if revision.len() == 40 && revision.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        Ok(revision)
    } else {
        Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "source revision must be a 40-character Git commit SHA",
        ))
    }
}

fn encode_url_path(path: &str) -> String {
    let mut output = String::with_capacity(path.len());
    for byte in path.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b'~' | b'/') {
            output.push(byte as char);
        } else {
            output.push_str(&format!("%{byte:02X}"));
        }
    }
    output
}

fn normalize_repository_url(url: &str) -> String {
    url.trim()
        .trim_end_matches('/')
        .trim_end_matches(".git")
        .to_string()
}

fn github_repository_parts(url: &str) -> Result<(String, String), String> {
    let normalized = normalize_repository_url(url);
    let path = normalized
        .strip_prefix("https://github.com/")
        .ok_or_else(|| "extra_skills_source must be an HTTPS GitHub repository URL".to_string())?;
    let parts = path.split('/').collect::<Vec<_>>();
    if parts.len() != 2
        || parts.iter().any(|part| {
            part.is_empty()
                || !part
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
        })
    {
        return Err("extra_skills_source must name one GitHub owner/repository".to_string());
    }
    Ok((parts[0].to_string(), parts[1].to_string()))
}

fn safe_skill_dir(id: &str) -> String {
    let mut output = String::new();
    let mut previous_dash = false;
    for ch in id.chars() {
        if ch.is_ascii_alphanumeric() {
            output.push(ch.to_ascii_lowercase());
            previous_dash = false;
        } else if !previous_dash {
            output.push('-');
            previous_dash = true;
        }
    }
    let output = output.trim_matches('-').to_string();
    if output.is_empty() {
        "skill".to_string()
    } else {
        output
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    #[cfg(unix)]
    use std::os::unix::fs::PermissionsExt;
    use std::time::{SystemTime, UNIX_EPOCH};

    #[test]
    fn bundled_anthropic_index_is_pinned_and_marks_document_skills() {
        let source = anthropic_source().unwrap();

        assert_eq!(source.name, "anthropic");
        assert_eq!(source.repository, ANTHROPIC_SKILLS_URL);
        assert_eq!(source.revision.len(), 40);
        let skill = |id: &str| source.skills.iter().find(|skill| skill.id == id).unwrap();
        assert!(skill("mcp-builder").redistributable);
        assert!(!skill("mcp-builder").description.is_empty());
        let restricted = source
            .skills
            .iter()
            .filter(|skill| !skill.redistributable)
            .map(|skill| skill.id.as_str())
            .collect::<Vec<_>>();
        assert_eq!(restricted, ["docx", "pdf", "pptx", "xlsx"]);
        assert_eq!(
            source.homepage("pdf"),
            format!("{ANTHROPIC_SKILLS_URL}/tree/{}/skills/pdf", source.revision)
        );
    }

    #[test]
    fn source_index_rejects_unsafe_ids_and_incomplete_entries() {
        let base = json!({
            "name": "test",
            "repository": "https://github.com/example/skills",
            "revision": "0123456789abcdef0123456789abcdef01234567",
            "skills": [{
                "id": "safe-skill", "name": "Safe", "description": "Safe skill",
                "license": "MIT", "redistributable": true
            }]
        });
        assert!(parse_source_index(&base).is_ok());

        for id in ["../escape", "nested/escape", "UPPER", "-dash"] {
            let mut unsafe_index = base.clone();
            unsafe_index["skills"][0]["id"] = json!(id);
            assert!(parse_source_index(&unsafe_index).is_err(), "{id}");
        }
        let mut no_license_flag = base.clone();
        no_license_flag["skills"][0]
            .as_object_mut()
            .unwrap()
            .remove("redistributable");
        assert!(parse_source_index(&no_license_flag).is_err());
        let mut branch = base;
        branch["revision"] = json!("main");
        assert!(parse_source_index(&branch).is_err());
    }

    #[test]
    fn installs_the_whole_github_skill_directory_at_the_pinned_revision() {
        let root = test_dir("jucode-github-skill-test");
        let source = github_source();
        let tree = json!({ "truncated": false, "tree": [
            { "path": "skills/demo", "type": "tree", "mode": "040000" },
            { "path": "skills/demo/SKILL.md", "type": "blob", "mode": "100644", "size": 30 },
            { "path": "skills/demo/scripts", "type": "tree", "mode": "040000" },
            { "path": "skills/demo/scripts/run me.sh", "type": "blob", "mode": "100755", "size": 9 },
            { "path": "skills/other/SKILL.md", "type": "blob", "mode": "100644", "size": 5 }
        ]});
        let revision = source.revision.clone();
        let fetched = std::cell::RefCell::new(Vec::new());
        let fetch = |url: &str, _limit: usize| -> io::Result<Vec<u8>> {
            fetched.borrow_mut().push(url.to_string());
            let raw = format!("https://raw.githubusercontent.com/example/skills/{revision}/");
            match url.strip_prefix(&raw) {
                None => Ok(tree.to_string().into_bytes()),
                Some("skills/demo/SKILL.md") => Ok(b"---\nname: demo\n---\n".to_vec()),
                Some("skills/demo/scripts/run%20me.sh") => Ok(b"echo ok\n".to_vec()),
                Some(other) => panic!("unexpected download {other}"),
            }
        };

        let dir = install_github_skill(&root, &source, &source.skills[0], &fetch).unwrap();

        assert_eq!(dir, root.join("demo"));
        assert_eq!(
            fetched.borrow()[0],
            format!("https://api.github.com/repos/example/skills/git/trees/{revision}?recursive=1")
        );
        assert_eq!(fetched.borrow().len(), 3);
        assert!(skill_installed(&root, "demo"));
        assert_eq!(
            fs::read_to_string(dir.join("scripts/run me.sh")).unwrap(),
            "echo ok\n"
        );
        #[cfg(unix)]
        assert_eq!(
            fs::metadata(dir.join("scripts/run me.sh"))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o755
        );
        assert_eq!(
            installed_source(&root, "demo").as_deref(),
            Some("https://github.com/example/skills")
        );

        // Another source's skill of the same name does not replace it; the
        // same source updates it.
        let mut other = github_source();
        other.repository = "https://github.com/someone/else".to_string();
        let error = install_github_skill(&root, &other, &other.skills[0], &fetch).unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::AlreadyExists);
        install_github_skill(&root, &source, &source.skills[0], &fetch).unwrap();
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn a_github_source_is_read_at_one_commit_with_each_skill_license() {
        let sha = "0123456789abcdef0123456789abcdef01234567";
        let api = "https://api.github.com/repos/example/skills";
        let raw = format!("https://raw.githubusercontent.com/example/skills/{sha}/skills/");
        let fetched = std::sync::Mutex::new(Vec::new());
        let fetch = |url: &str, _limit: usize| -> io::Result<Vec<u8>> {
            fetched.lock().unwrap().push(url.to_string());
            let body = if url == api {
                json!({ "default_branch": "main", "license": { "spdx_id": "MIT" } }).to_string()
            } else if url == format!("{api}/branches/main") {
                json!({ "commit": { "sha": sha } }).to_string()
            } else if url == format!("{api}/git/trees/{sha}?recursive=1") {
                json!({ "tree": [
                    { "path": "skills/open/SKILL.md", "type": "blob" },
                    { "path": "skills/closed/SKILL.md", "type": "blob" },
                    { "path": "skills/apache/SKILL.md", "type": "blob" }
                ]})
                .to_string()
            } else {
                match url.strip_prefix(&raw) {
                    Some("open/SKILL.md") => "---\nname: Open\n---\n".to_string(),
                    Some("closed/SKILL.md") => {
                        "---\nname: Closed\nlicense: Proprietary. LICENSE.txt has complete terms\n---\n"
                            .to_string()
                    }
                    Some("apache/SKILL.md") => {
                        "---\nname: Apache\nlicense: Apache License 2.0\n---\n".to_string()
                    }
                    _ => panic!("unexpected download {url}"),
                }
            };
            Ok(body.into_bytes())
        };

        let source =
            read_github_source("Example", "https://github.com/example/skills", &fetch).unwrap();

        assert_eq!(source.revision, sha);
        assert_eq!(source.dir, "skills");
        let skill = |id: &str| source.skills.iter().find(|skill| skill.id == id).unwrap();
        assert_eq!(
            (
                skill("open").license.as_str(),
                skill("open").redistributable
            ),
            ("MIT", true)
        );
        assert!(!skill("closed").redistributable);
        assert!(skill("apache").redistributable);
        assert!(!fetched
            .lock()
            .unwrap()
            .iter()
            .any(|url| url.contains("/HEAD")));
    }

    #[test]
    fn only_open_source_licenses_are_offered() {
        for license in [
            "MIT",
            "MIT License",
            "Apache-2.0",
            "apache license, version 2.0",
            "BSD-3-Clause",
        ] {
            assert!(open_license(license), "{license}");
        }
        for license in [
            "",
            "Proprietary",
            "Complete terms in LICENSE.txt",
            "NOASSERTION",
        ] {
            assert!(!open_license(license), "{license}");
        }
    }

    #[test]
    fn github_install_rejects_escaping_paths_and_keeps_the_existing_skill() {
        let root = test_dir("jucode-github-escape-test");
        let installed = root.join("demo");
        fs::create_dir_all(&installed).unwrap();
        fs::write(installed.join("SKILL.md"), "old content").unwrap();
        let source = github_source();
        for (path, kind) in [
            ("skills/demo/../../escape", "blob"),
            ("skills/demo/link", "commit"),
        ] {
            let tree = json!({ "tree": [
                { "path": "skills/demo/SKILL.md", "type": "blob", "mode": "100644", "size": 3 },
                { "path": path, "type": kind, "mode": "100644", "size": 3 }
            ]});
            let fetch = |_: &str, _: usize| Ok(tree.to_string().into_bytes());

            let error = install_github_skill(&root, &source, &source.skills[0], &fetch)
                .unwrap_err()
                .to_string();

            assert!(error.contains(path), "{error}");
        }
        let no_skill_file = json!({ "tree": [
            { "path": "skills/demo/README.md", "type": "blob", "mode": "100644", "size": 3 }
        ]});
        let fetch = |url: &str, _: usize| {
            Ok(if url.contains("api.github.com") {
                no_skill_file.to_string().into_bytes()
            } else {
                b"doc".to_vec()
            })
        };
        let error = install_github_skill(&root, &source, &source.skills[0], &fetch).unwrap_err();
        assert!(error.to_string().contains("SKILL.md"), "{error}");
        assert_eq!(
            fs::read_to_string(installed.join("SKILL.md")).unwrap(),
            "old content"
        );
        assert_eq!(fs::read_dir(&root).unwrap().count(), 1);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn lifecycle_enable_disable_and_uninstall_updates_state() {
        let root = test_dir("jucode-skill-lifecycle-test");
        let installed = root.join("skills/review");
        fs::create_dir_all(&installed).unwrap();
        fs::write(
            installed.join("SKILL.md"),
            "---\nname: review\ndescription: Review\n---\n",
        )
        .unwrap();

        assert!(skill_installed(&root.join("skills"), "review"));
        assert!(is_skill_path_enabled(&root, &installed.join("SKILL.md")).unwrap());
        set_skill_enabled(&root, "review", false).unwrap();
        assert!(!is_skill_path_enabled(&root, &installed.join("SKILL.md")).unwrap());
        assert_eq!(installed_skill_ids(&root).unwrap(), ["review (disabled)"]);
        set_skill_enabled(&root, "review", true).unwrap();
        assert!(is_skill_path_enabled(&root, &installed.join("SKILL.md")).unwrap());
        assert!(uninstall_skill(&root, "review").unwrap());
        assert!(!skill_installed(&root.join("skills"), "review"));
        assert!(!uninstall_skill(&root, "review").unwrap());
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn package_download_has_a_hard_size_limit() {
        let error = read_bounded(
            io::Cursor::new(vec![0_u8; MAX_PACKAGE_BYTES + 1]),
            MAX_PACKAGE_BYTES,
        )
        .unwrap_err();
        assert!(error.to_string().contains("byte limit"));
    }

    fn github_source() -> SkillSource {
        SkillSource {
            name: "example".to_string(),
            repository: "https://github.com/example/skills".to_string(),
            revision: "0123456789abcdef0123456789abcdef01234567".to_string(),
            dir: "skills".to_string(),
            skills: vec![SourceSkill {
                id: "demo".to_string(),
                name: "Demo".to_string(),
                description: "Demo skill".to_string(),
                tags: Vec::new(),
                license: "MIT".to_string(),
                redistributable: true,
            }],
        }
    }

    fn test_dir(prefix: &str) -> PathBuf {
        std::env::temp_dir().join(format!(
            "{}-{}",
            prefix,
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ))
    }

    /// Network: GitHub skill repositories of both layouts list their skills
    /// with metadata.
    #[test]
    #[ignore]
    fn github_sources_list_skills() {
        for (name, repository) in [
            ("Superpowers", "https://github.com/obra/superpowers"),
            (
                "Composio",
                "https://github.com/ComposioHQ/awesome-claude-skills",
            ),
        ] {
            let source = fetch_github_source(name, repository).unwrap();
            eprintln!(
                "{name}: dir={:?} {} skills",
                source.dir,
                source.skills.len()
            );
            eprintln!(
                "  revision={} offered={}/{}",
                source.revision,
                source
                    .skills
                    .iter()
                    .filter(|skill| skill.redistributable)
                    .count(),
                source.skills.len()
            );
            for skill in source.skills.iter().take(3) {
                eprintln!(
                    "  {} | {} | {} | {:.60}",
                    skill.id, skill.name, skill.license, skill.description
                );
            }
            assert!(!source.skills.is_empty());
            assert!(source
                .skills
                .iter()
                .any(|skill| !skill.description.is_empty()));
        }
    }
}
