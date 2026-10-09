#!/usr/bin/env ruby
# frozen_string_literal: true

# Open a file by its fileregister durable id or aka.
#
#   ref://id/<id-or-aka>   — resolve via the register CLI, open the file, and
#                            print its file:// URL.
#
# Environment (set them under script_env in milan's config.yaml):
#   GRUBBER_NOTES or GRUBBER_SET  — where the register index lives
#   REGISTER_BIN                  — path to the register binary if it is not on PATH

require "uri"
require "open3"

# milan gives scripts a UTF-8 locale, but older milans and other launchers do
# not; without one, Ruby reads register's output as US-ASCII and fails on the
# first non-ASCII path ("Pässe.md").
Encoding.default_external = Encoding::UTF_8

REGISTER = ENV["REGISTER_BIN"] || "register"

def file_url(path)
  encoded = URI::RFC2396_Parser.new.escape(path).gsub("%2F", "/")
  "file://#{encoded}"
end

key = ARGV[0].to_s
if key.empty?
  warn "usage: id/<id-or-aka>"
  exit 1
end

# An unknown key and a broken environment both exit non-zero; register's stderr
# is what tells them apart, so hand it through verbatim.
begin
  out, err, status = Open3.capture3(REGISTER, "resolve", key)
rescue Errno::ENOENT
  warn "register not found (set REGISTER_BIN or add it to PATH)"
  exit 1
end

path = out.strip
unless status.success? && !path.empty?
  warn err.strip.empty? ? "not found: #{key}" : err.strip
  exit 1
end

# Resolved, but nothing there: the record outlived the file (register repair).
unless File.exist?(path)
  warn "record resolves to a missing file: #{path}"
  exit 1
end

system("open", path)
puts file_url(path)
