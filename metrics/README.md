# Usage snapshots

This directory contains aggregate GitHub signals captured weekly by
`.github/workflows/metrics-snapshot.yml`. They measure release and repository
activity without adding telemetry to the scribe CLI.

Each `YYYY-MM-DD.json` file uses schema version 1 and contains:

- cumulative download counts for real `scribe_*_{darwin,linux}_{amd64,arm64}.tar.gz`
  release archives, grouped by release and asset;
- stars, forks, and separate open issue and pull-request counts;
- GitHub's rolling 14-day clone and page-view totals and daily breakdowns.

Traffic windows overlap, so do not add clone or view totals across snapshots.
Run `scripts/metrics-report` from anywhere in the checkout for a compact trend
report. The script requires `jq`.
