#!/usr/bin/env ruby
# frozen_string_literal: true

# Milan Example Script: hello.rb
# URL: http://mi.lan/hello/world
# Argument is passed as ARGV[0]

argument = ARGV[0] || 'stranger'

# macOS Notification — inspect escaped Quotes/Backslashes, sonst wäre das
# URL-Argument AppleScript-Injection (inkl. do shell script)
system(
  'osascript', '-e',
  "display notification #{"Hello, #{argument}!".inspect} with title \"Milan\""
)

# Output is returned as HTTP response
puts "[#{Time.now}] hello.rb executed with: #{argument}"
