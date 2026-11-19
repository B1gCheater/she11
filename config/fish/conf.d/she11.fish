# Install as ~/.config/fish/conf.d/she11.fish on the remote Linux host.
# Use the current theme's remote hostname color for she11 PTY sessions.
status is-interactive; or return
set -q SHE11_SESSION[1]; and test -n "$SHE11_SESSION"; or return

function __she11_remote_host_color --on-event fish_prompt
    if set -q fish_color_host_remote
        # Process-local only: do not change universal settings or SSH variables.
        set -g fish_color_host $fish_color_host_remote
    end
end

__she11_remote_host_color
