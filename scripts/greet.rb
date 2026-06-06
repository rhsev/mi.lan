#!/usr/bin/env ruby
# frozen_string_literal: true

# Milan Example: greet.rb
# Demonstrates the MILAN_PROMPT multi-step workflow protocol.
#
# When triggered as a stream (e.g. via /mini/stream/greet) this script
# prints a short intro, then emits a `MILAN_PROMPT` JSON line. Stage
# turns that line into an input field and, on submit, calls the named
# `action` URL with the user's input appended as the next path segment.
#
# Result for the user: click button → see context → type a name → hello.rb
# fires on the Mac with that name and shows the macOS notification.

puts '👋 Hello-World greeter ready'
puts 'Type a name and hello.rb will greet them.'
puts ''
puts 'MILAN_PROMPT {"label":"Name to greet","action":"/mini/hello"}'
