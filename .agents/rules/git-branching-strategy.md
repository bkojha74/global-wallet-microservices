# Git Branching Strategy & Merge Rules

## Branching Policy
1. **Feature / Bugfix Branches (`feature/*`, `bugfix/*`)**:
   - MUST ONLY target `develop` branch for PRs and merges.
   - CANNOT merge directly into `main`.

2. **Develop Branch (`develop`)**:
   - Integrates features and bugfixes.
   - PRs from `develop` target `main` when ready for release.

3. **Main Branch (`main`)**:
   - Production-ready stable branch.
   - ONLY accepts pull requests originating from `develop` (or hotfix branches).
