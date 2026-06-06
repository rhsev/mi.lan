#!/usr/bin/env ruby
# frozen_string_literal: true

# Milan Stream Demo: counter.rb
# Demonstrates streaming output — each line appears live in the browser.
#
# Usage: GET /stream/counter      → counts to 5
#        GET /stream/counter/8    → counts to 8

$stdout.sync = true  # Required: flush after each puts (no terminal buffering)

count = (ARGV[0].to_s.strip.empty? ? 5 : ARGV[0].to_i).clamp(1, 30)
ts    = -> { Time.now.strftime('%H:%M:%S') }

puts "[#{ts.call}] Starting — counting to #{count}"

count.times do |i|
  sleep 0.8
  puts "[#{ts.call}] Step #{i + 1}/#{count}"
end

puts "[#{ts.call}] Done."
