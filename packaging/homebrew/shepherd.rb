class Shepherd < Formula
  desc "Resilient orchestrator for batch coding agents (Claude, Antigravity, Codex)"
  homepage "https://github.com/erinwnash/Shepherd"
  url "https://github.com/erinwnash/Shepherd/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "Apache-2.0"
  head "https://github.com/erinwnash/Shepherd.git", branch: "main"

  depends_on "go" => :build

  def install
    ENV["CGO_ENABLED"] = "0"
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}"), "./cmd/shepherd"
  end

  service do
    run [opt_bin/"shepherd", "start", "-f"]
    keep_alive true
    log_path var/"log/shepherd.log"
    error_log_path var/"log/shepherd.err.log"
  end

  test do
    assert_match "shepherd version", shell_output("#{bin}/shepherd version")
  end
end
