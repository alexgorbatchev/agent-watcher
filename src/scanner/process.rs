use super::{check_cancel, paths::clean_path};
use std::{
    collections::{BTreeMap, HashSet},
    io,
    path::PathBuf,
};
use sysinfo::{Pid, ProcessRefreshKind, ProcessStatus, ProcessesToUpdate, System, UpdateKind};
use tokio_util::sync::CancellationToken;

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ProcessMeta {
    pub pid: i32,
    pub name: String,
    pub create_time: i64,
    pub alive: bool,
    pub cwd: PathBuf,
    pub cmdline: Vec<String>,
}
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ProcessInfo {
    pub pid: i32,
    pub cwd: PathBuf,
    pub create_time: i64,
}

/// Captured process information, independent of subsequent process churn.
#[derive(Clone, Debug, Default)]
pub struct ProcessSnapshot {
    processes: BTreeMap<i32, ProcessMeta>,
    pi: Vec<ProcessInfo>,
    codex: Vec<ProcessInfo>,
    opencode: Vec<ProcessInfo>,
    agy: Vec<ProcessInfo>,
    parents: BTreeMap<i32, i32>,
}
pub trait SnapshotProvider: Send + Sync {
    fn snapshot(&self, token: &CancellationToken) -> io::Result<ProcessSnapshot>;
}
impl<F> SnapshotProvider for F
where
    F: Fn(&CancellationToken) -> io::Result<ProcessSnapshot> + Send + Sync,
{
    fn snapshot(&self, token: &CancellationToken) -> io::Result<ProcessSnapshot> {
        self(token)
    }
}
#[derive(Clone, Copy, Debug, Default)]
pub struct LiveProcessProvider;
impl SnapshotProvider for LiveProcessProvider {
    fn snapshot(&self, token: &CancellationToken) -> io::Result<ProcessSnapshot> {
        ProcessSnapshot::capture(token)
    }
}
fn basename(name: &str) -> String {
    name.rsplit(['/', '\\'])
        .next()
        .unwrap_or(name)
        .to_ascii_lowercase()
        .trim_end_matches(".exe")
        .into()
}
fn plausible(name: &str) -> bool {
    matches!(
        name,
        "pi" | "codex" | "codex-tui" | "codex-cli" | "opencode" | "claude" | "agy"
    ) || interpreter(name)
}
fn interpreter(name: &str) -> bool {
    ["node", "bun", "electron"]
        .iter()
        .any(|prefix| name.starts_with(prefix))
}
impl ProcessSnapshot {
    pub fn capture(token: &CancellationToken) -> io::Result<Self> {
        Self::capture_live(token, None)
    }
    /// Capture metadata and classify only processes accepted by the PID predicate.
    pub fn capture_filtered(
        token: &CancellationToken,
        mut include: impl FnMut(i32) -> bool,
    ) -> io::Result<Self> {
        Self::capture_live(token, Some(&mut include))
    }
    fn capture_live(
        token: &CancellationToken,
        mut include: Option<&mut dyn FnMut(i32) -> bool>,
    ) -> io::Result<Self> {
        check_cancel(token)?;
        let selected_pids = if let Some(include) = &mut include {
            let mut selected = Vec::new();
            for pid in live_pids()? {
                check_cancel(token)?;
                if include(pid) {
                    selected.push(Pid::from_u32(pid as u32));
                }
            }
            check_cancel(token)?;
            Some(selected)
        } else {
            None
        };
        let mut system = System::new();
        system.refresh_processes_specifics(
            selected_pids
                .as_deref()
                .map_or(ProcessesToUpdate::All, ProcessesToUpdate::Some),
            true,
            ProcessRefreshKind::nothing().without_tasks(),
        );
        check_cancel(token)?;
        let mut selected = Vec::new();
        let mut classification = BTreeMap::new();
        for (pid, process) in system.processes() {
            check_cancel(token)?;
            let name = classification_name(*pid, process.name());
            if plausible(&name) && name != "claude" {
                selected.push(*pid);
                if let Ok(pid) = i32::try_from(pid.as_u32()) {
                    classification.insert(pid, name);
                }
            }
        }
        if !selected.is_empty() {
            system.refresh_processes_specifics(
                ProcessesToUpdate::Some(&selected),
                true,
                ProcessRefreshKind::nothing()
                    .without_tasks()
                    .with_cmd(UpdateKind::Always)
                    .with_cwd(UpdateKind::Always),
            );
        }
        let mut processes = Vec::with_capacity(system.processes().len());
        let mut parents = BTreeMap::new();
        for (pid, p) in system.processes() {
            check_cancel(token)?;
            let Ok(pid) = i32::try_from(pid.as_u32()) else {
                continue;
            };
            if let Some(parent) = p
                .parent()
                .and_then(|parent| i32::try_from(parent.as_u32()).ok())
            {
                parents.insert(pid, parent);
            }
            processes.push(ProcessMeta {
                pid,
                name: p.name().to_string_lossy().into_owned(),
                create_time: i64::try_from(p.start_time())
                    .unwrap_or(i64::MAX)
                    .saturating_mul(1000),
                alive: !matches!(p.status(), ProcessStatus::Zombie | ProcessStatus::Dead),
                cwd: p.cwd().map_or_else(PathBuf::new, |p| p.to_path_buf()),
                cmdline: p
                    .cmd()
                    .iter()
                    .map(|s| s.to_string_lossy().into_owned())
                    .collect(),
            });
        }
        Ok(Self::classify(processes, parents, &classification))
    }
    pub fn from_processes(processes: impl IntoIterator<Item = ProcessMeta>) -> Self {
        Self::from_processes_with_parents(processes, [])
    }
    /// Construct a snapshot with optional (child PID, parent PID) relationships.
    pub fn from_processes_with_parents(
        processes: impl IntoIterator<Item = ProcessMeta>,
        parents: impl IntoIterator<Item = (i32, i32)>,
    ) -> Self {
        Self::classify(processes, parents.into_iter().collect(), &BTreeMap::new())
    }
    fn classify(
        processes: impl IntoIterator<Item = ProcessMeta>,
        parents: BTreeMap<i32, i32>,
        classification: &BTreeMap<i32, String>,
    ) -> Self {
        let mut snapshot = Self {
            parents,
            ..Self::default()
        };
        for mut p in processes {
            if !p.cwd.as_os_str().is_empty() {
                p.cwd = clean_path(&p.cwd)
            }
            snapshot.processes.insert(p.pid, p);
        }
        for p in snapshot.processes.values() {
            if p.pid <= 0 || !p.alive || p.cwd.as_os_str().is_empty() {
                continue;
            }
            let name = classification
                .get(&p.pid)
                .cloned()
                .unwrap_or_else(|| basename(&p.name));
            if !plausible(&name) {
                continue;
            }
            let names: Vec<_> = if interpreter(&name) {
                p.cmdline.iter().map(|s| basename(s)).collect()
            } else {
                vec![name]
            };
            let info = ProcessInfo {
                pid: p.pid,
                cwd: p.cwd.clone(),
                create_time: p.create_time,
            };
            if names.iter().any(|n| n == "pi") {
                snapshot.pi.push(info.clone())
            }
            if names
                .iter()
                .any(|n| matches!(n.as_str(), "codex" | "codex-cli" | "codex-tui"))
            {
                snapshot.codex.push(info.clone())
            }
            if names.iter().any(|n| n == "opencode") {
                snapshot.opencode.push(info.clone())
            }
            if names.iter().any(|n| n == "agy") {
                snapshot.agy.push(info)
            }
        }
        snapshot
    }
    pub fn process(&self, pid: i32) -> Option<&ProcessMeta> {
        self.processes.get(&pid)
    }
    /// Iterate the captured metadata without re-reading a changing process table.
    pub fn processes(&self) -> impl ExactSizeIterator<Item = &ProcessMeta> {
        self.processes.values()
    }
    /// Associate an explicitly selected harness with a captured live process.
    ///
    /// The caller supplies its observed working directory and exact captured start
    /// time. This permits alias/interpreter launches without changing process names
    /// or command lines. A subsequent declaration replaces the same PID's candidate.
    /// Claude Code discovers processes through its own PID descriptors instead.
    pub fn declare_candidate(
        &mut self,
        harness: crate::Harness,
        mut candidate: ProcessInfo,
    ) -> io::Result<()> {
        if candidate.pid <= 0
            || candidate.cwd.as_os_str().is_empty()
            || !self.process(candidate.pid).is_some_and(|process| {
                process.alive && process.create_time == candidate.create_time
            })
        {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "candidate must identify a captured live process with its exact start time and working directory",
            ));
        }
        let candidates = match harness {
            crate::Harness::Pi => &mut self.pi,
            crate::Harness::Codex => &mut self.codex,
            crate::Harness::Opencode => &mut self.opencode,
            crate::Harness::Agy => &mut self.agy,
            crate::Harness::ClaudeCode => {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    "Claude Code uses PID descriptors",
                ));
            }
        };
        candidate.cwd = clean_path(&candidate.cwd);
        if let Some(existing) = candidates.iter_mut().find(|p| p.pid == candidate.pid) {
            *existing = candidate;
        } else {
            candidates.push(candidate);
        }
        Ok(())
    }
    pub fn is_process_alive(&self, pid: i32) -> bool {
        pid > 0 && self.process(pid).is_some_and(|p| p.alive)
    }
    pub fn verify_process_match(&self, pid: i32, expected_start: i64) -> bool {
        self.is_process_alive(pid)
            && self.process(pid).is_some_and(|p| {
                expected_start <= 0
                    || p.create_time <= 0
                    || p.create_time.abs_diff(expected_start) < 60_000
            })
    }
    pub fn pi_processes(&self) -> &[ProcessInfo] {
        &self.pi
    }
    pub fn codex_processes(&self) -> &[ProcessInfo] {
        &self.codex
    }
    pub(super) fn top_level_codex_processes(&self) -> impl Iterator<Item = &ProcessInfo> {
        let pids: HashSet<_> = self.codex.iter().map(|process| process.pid).collect();
        self.codex.iter().filter(move |process| {
            self.parents
                .get(&process.pid)
                .is_none_or(|parent| !pids.contains(parent))
        })
    }
    pub fn opencode_processes(&self) -> &[ProcessInfo] {
        &self.opencode
    }
    pub fn agy_processes(&self) -> &[ProcessInfo] {
        &self.agy
    }
}

fn classification_name(pid: Pid, name: &std::ffi::OsStr) -> String {
    let name = basename(&name.to_string_lossy());
    #[cfg(target_os = "linux")]
    if !plausible(&name)
        && let Ok(executable) = std::fs::read_link(format!("/proc/{}/exe", pid.as_u32()))
    {
        return basename(&executable.to_string_lossy());
    }
    #[cfg(not(target_os = "linux"))]
    let _ = pid;
    name
}

#[cfg(target_os = "linux")]
fn live_pids() -> io::Result<Vec<i32>> {
    Ok(std::fs::read_dir("/proc")?
        .filter_map(|entry| entry.ok()?.file_name().to_str()?.parse::<i32>().ok())
        .filter(|pid| *pid > 0)
        .collect())
}

#[cfg(target_os = "macos")]
fn live_pids() -> io::Result<Vec<i32>> {
    // Passing a null buffer asks libproc for the approximate number of PIDs.
    let estimated = unsafe { libc::proc_listallpids(std::ptr::null_mut(), 0) };
    let mut capacity = estimated.saturating_add(64).max(64) as usize;
    loop {
        let mut pids = vec![0; capacity];
        let bytes =
            i32::try_from(std::mem::size_of_val(pids.as_slice())).map_err(io::Error::other)?;
        // pids contains bytes of writable, correctly aligned PID storage.
        let count = unsafe { libc::proc_listallpids(pids.as_mut_ptr().cast(), bytes) };
        if count < 0 {
            return Err(io::Error::last_os_error());
        }
        if (count as usize) < capacity {
            pids.truncate(count as usize);
            pids.retain(|pid| *pid > 0);
            return Ok(pids);
        }
        capacity *= 2;
    }
}

#[cfg(not(any(target_os = "linux", target_os = "macos")))]
fn live_pids() -> io::Result<Vec<i32>> {
    let mut system = System::new();
    system.refresh_processes_specifics(
        ProcessesToUpdate::All,
        true,
        ProcessRefreshKind::nothing().without_tasks(),
    );
    Ok(system
        .processes()
        .keys()
        .filter_map(|pid| i32::try_from(pid.as_u32()).ok().filter(|pid| *pid > 0))
        .collect())
}

pub(super) fn pair_process(
    cwd: &std::path::Path,
    created: i64,
    recent: i64,
    processes: &[ProcessInfo],
    used: &mut std::collections::HashSet<i32>,
) -> Option<ProcessInfo> {
    let found = processes
        .iter()
        .filter(|p| !used.contains(&p.pid) && p.cwd == clean_path(cwd))
        .filter_map(|p| {
            let mut difference = i128::from(created) - i128::from(p.create_time);
            let resumed = i128::from(recent) - i128::from(p.create_time);
            if difference < -5000 && resumed >= -5000 {
                difference = resumed
            }
            (difference >= -5000).then_some((difference.unsigned_abs(), p))
        })
        .min_by_key(|(distance, _)| *distance)
        .map(|(_, p)| p.clone());
    if let Some(p) = &found {
        used.insert(p.pid);
    }
    found
}
