# widget-na — push the next actions of the current directory to the board.
#
# A slow state, not a job: no ttl, so the tile stands until the next cd. The
# board shows the clock time of that last push — "four minutes ago" is the
# normal and correct answer here, not an error.
#
# Install: source it from config.fish (an event handler has to exist before
# the event, so the functions/ autoload directory would be too late):
#
#   source /path/to/mi.lan/scripts/fish/widget-na.fish
#   ln -s /path/to/mi.lan/scripts/widget ~/bin/widget     # client on PATH
#
# `set -U widget_na_target other` moves it to a different tile.

function __widget_na_push --argument-names target
    # --color keeps na's own highlighting; the board renders the ANSI.
    set -l out (na next --color --no-pager 2>/dev/null | head -n 6 | string collect)
    test -n "$out"; or return
    printf '%s\n' $out | widget $target --title (basename $PWD)
end

function widget-na --on-variable PWD --description 'Push next actions to the widget board'
    status is-interactive; or return
    command -q na widget; or return

    set -l target na
    set -q widget_na_target; and set target $widget_na_target

    # Fire and forget: na walks the tree, the prompt should not wait for it.
    __widget_na_push $target &
    disown
end
