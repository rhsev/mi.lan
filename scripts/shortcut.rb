#!/usr/bin/env ruby
# frozen_string_literal: true

# Run a macOS Shortcut
# URL: http://mi.lan:8080/shortcut/Name
#      http://mi.lan:8080/shortcut/Name/input%20text

# ARGV[0] = "Name" or "Name/input text" (URL-decoded by Milan)
arg = ARGV[0] || ''
parts = arg.split('/', 2)

shortcut_name = parts[0]
shortcut_input = parts[1]

if shortcut_name.empty?
  warn "No shortcut name provided"
  exit 1
end

# Run shortcut via shortcuts CLI
cmd = ['shortcuts', 'run', shortcut_name]
cmd += ['--input-path', '-'] if shortcut_input

if shortcut_input
  # With input: pass via stdin
  IO.popen(cmd, 'r+') do |io|
    io.write(shortcut_input)
    io.close_write
    print io.read
  end
else
  # Without input
  system(*cmd)
end

exit($?.exitstatus || 0)
