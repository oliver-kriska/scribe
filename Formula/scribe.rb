# Homebrew formula for scribe.
#
# Lives in a tap (e.g. `oliver-kriska/scribe`). Publish with:
#   brew tap-new <user>/scribe
#   cp Formula/scribe.rb $(brew --repo <user>/scribe)/Formula/
#   brew install <user>/scribe/scribe
#
# The URL + SHA256 placeholders are refreshed by goreleaser on every tagged
# release (see .goreleaser.yml → brews:).
class Scribe < Formula
  desc     "LLM-managed personal knowledge base tooling"
  homepage "https://github.com/oliver-kriska/scribe"
  version  "0.0.0"
  license  "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/oliver-kriska/scribe/releases/download/v#{version}/scribe_#{version}_darwin_arm64.tar.gz"
      sha256 "REPLACE_ME_DARWIN_ARM64"
    else
      url "https://github.com/oliver-kriska/scribe/releases/download/v#{version}/scribe_#{version}_darwin_amd64.tar.gz"
      sha256 "REPLACE_ME_DARWIN_AMD64"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/oliver-kriska/scribe/releases/download/v#{version}/scribe_#{version}_linux_arm64.tar.gz"
      sha256 "REPLACE_ME_LINUX_ARM64"
    else
      url "https://github.com/oliver-kriska/scribe/releases/download/v#{version}/scribe_#{version}_linux_amd64.tar.gz"
      sha256 "REPLACE_ME_LINUX_AMD64"
    end
  end

  depends_on "git"
  depends_on "sqlite"

  # ccrider is the Claude-session recorder scribe reads via FTS5. It ships
  # from neilberkman's tap; brew auto-taps on install.
  depends_on "neilberkman/tap/ccrider"

  # Not declared here (no brew formula exists): `claude` (install via
  # `curl -fsSL https://claude.ai/install.sh | bash` or npm), `qmd`
  # (semantic-search over the KB, install separately), `trafilatura`
  # (optional, pip/pipx), `jq` and `fzf` (optional).

  def install
    bin.install "scribe"
  end

  def caveats
    <<~EOS
      Runtime dependencies not on Homebrew — install these separately:
        * claude     (Claude Code CLI)
                     curl -fsSL https://claude.ai/install.sh | bash
        * qmd        (semantic search over the KB)
                     npm install -g @tobilu/qmd
        * trafilatura (optional, URL → markdown)
                     pipx install trafilatura
        * jq, fzf    (optional)
                     brew install jq fzf

      Already installed by brew as dependencies: git, sqlite, ccrider.

      After installing:
        scribe init --path ~/my-kb
        scribe cron install           # macOS: LaunchAgents
                                      # Linux: prints crontab lines to paste

      macOS — Full Disk Access for `scribe capture` (iMessage):
        scribe fda                    # opens the FDA pane and walks you through
                                      # use drag-and-drop from Finder if the
                                      # "+ / Cmd-Shift-G" flow fails to register

      Heads-up: until scribe ships with Developer ID codesigning, the FDA grant
      is tied to the Cellar path + binary cdhash (both change on every
      `brew upgrade scribe`). After an upgrade, `scribe capture` will start
      failing with "operation not permitted" — just re-run `scribe fda`.
      `scribe doctor` flags this situation explicitly.
    EOS
  end

  # post_install runs on fresh install and on `brew upgrade`. We use it to
  # surface the FDA-is-now-broken message specifically on upgrade, since brew
  # does not re-print `caveats` during upgrades and users otherwise hit a
  # silent `capture` failure the next time cron fires.
  def post_install
    return unless OS.mac?
    # The stable symlink at HOMEBREW_PREFIX/bin/scribe survives upgrades, but
    # the Cellar target behind it (and its cdhash) changes. Any prior TCC
    # grant is keyed to the *previous* Cellar inode and is therefore invalid.
    ohai "Homebrew upgraded scribe to #{version}."
    ohai "If iMessage capture was working before, macOS Full Disk Access is now"
    ohai "invalidated (TCC is keyed to the binary cdhash, not the install path)."
    ohai "Re-run:  scribe fda"
  end

  test do
    assert_match "scribe", shell_output("#{bin}/scribe --help 2>&1")
  end
end
