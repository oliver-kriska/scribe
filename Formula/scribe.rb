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

  # Required by `scribe triage` (keyword expansion + preview). Optional deps
  # (trafilatura, jq, fzf) are not listed so brew doesn't pull them on every
  # install — the README explains how to add them if you use capture/triage.

  def install
    bin.install "scribe"
  end

  def caveats
    <<~EOS
      scribe expects several runtime dependencies beyond brew-installed ones:
        * claude     (Claude Code CLI — https://claude.com/claude-code)
        * ccrider    (Claude session recorder)
        * qmd        (semantic search over the KB)
        * trafilatura (optional, URL → markdown)
        * jq, fzf   (optional)

      After installing:
        scribe init --path ~/my-kb
        scribe cron install           # macOS: LaunchAgents
                                      # Linux: prints crontab lines to paste

      On macOS, grant Full Disk Access to #{bin}/scribe in
      System Settings → Privacy & Security → Full Disk Access so that
      `scribe capture` can read ~/Library/Messages/chat.db.
    EOS
  end

  test do
    assert_match "scribe", shell_output("#{bin}/scribe --help 2>&1")
  end
end
