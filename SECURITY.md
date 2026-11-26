# Security policy

## Reporting

Report suspected vulnerabilities privately to the maintainers through GitHub's
private vulnerability reporting on this repository (Security → Report a
vulnerability). Do not open a public issue for an unfixed problem.

Include the version or commit, the affected configuration
(`VcrGpuCluster`, workload manifest), and what an attacker gains.

## Scope worth thinking about

This operator runs an admission webhook that can rewrite Pods and a cluster-scoped
device plugin that runs with elevated permissions on selected nodes. Reports about
either are in scope:

- admission bypass: a GPU-requesting Pod admitted without translation, or a
  translated Pod that still reaches a real device;
- credential exposure: Secret values in annotations, status, events, logs, or
  error messages (the projection path is the one that touches them);
- privilege escalation through the device-plugin ServiceAccount or its SCC binding;
- webhook availability: anything that lets an unprivileged caller block Pod
  creation in admitted namespaces.

## Supported versions

Fixes land on `main`. There are no maintained release branches yet; use the latest
tag.
