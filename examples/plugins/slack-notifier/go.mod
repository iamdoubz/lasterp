// The slack-notifier example (WP-3.2b). A module of its own, like every
// plugin: its wasip1 build constraints and its PDK dependency stay out of the
// server's build, and an author can copy this directory and start.
module example.com/slack-notifier

go 1.26.7

require github.com/extism/go-pdk v1.1.3
