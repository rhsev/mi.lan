#!/usr/bin/env ruby
# frozen_string_literal: true

# ============================================================================
# Milan - Minimalist Script Executor for macOS
# ============================================================================
#
# Receives HTTP requests and executes local scripts.
# Designed for Tailscale networks with IP-based authentication.
#
# URL schema: http://mi.lan/script_name/argument
# Example:    http://mi.lan/hello/world  ->  ./scripts/hello.rb "world"
#
# Config:  config.yaml
# Scripts: ./scripts/*.rb
#
# Start: ruby milan.rb
# ============================================================================

require 'async'
require 'async/http/endpoint'
require 'async/http/server'
require 'protocol/http/body/writable'
require 'yaml'
require 'fileutils'
require 'json'
require 'timeout'
require 'uri'
require 'cgi'
require 'open3'

# Disable DNS reverse lookup (saves ~80-100ms per request)
require 'socket'
BasicSocket.do_not_reverse_lookup = true

module Milan
  VERSION          = '1.0.0'
  SCRIPT_EXTENSIONS = %w[.rb .sh .py].freeze
  JOBS_DIR         = File.join(__dir__, 'data', 'jobs')
  JOBS_MUTEX       = Mutex.new

  # ==========================================================================
  # Config - Loads configuration from YAML
  # ==========================================================================
  class Config
    attr_reader :port, :allowed_ips, :scripts_dir, :cheaters_dir, :notes, :cron_interval

    def initialize(config_path = nil)
      config_path ||= File.join(__dir__, 'config.yaml')
      @config = YAML.load_file(config_path)

      milan_config = @config['milan'] || {}
      @port = milan_config['port'] || 8080
      @allowed_ips = Array(milan_config['allowed_ips'])
      @scripts_dir   = milan_config['scripts_dir'] || './scripts'
      @cheaters_dir  = milan_config['cheaters_dir'].to_s.strip
      @notes         = Array(milan_config['notes'])
      @cron_interval = (milan_config['cron_interval'] || 300).to_i

      # Create scripts directory if missing
      FileUtils.mkdir_p(scripts_dir)
    rescue Errno::ENOENT
      raise "Config not found: #{config_path}"
    end

    # Check if IP is allowed
    # Supports wildcards: "192.168.1.*"
    def allowed?(ip)
      # Always allow localhost (for local testing)
      return true if ip == '127.0.0.1' || ip == '::1'

      allowed_ips.any? do |allowed|
        if allowed.include?('*')
          # Wildcard: "192.168.1.*" matches "192.168.1.100"
          pattern = allowed.gsub('.', '\.').gsub('*', '\d+')
          ip.match?(/\A#{pattern}\z/)
        else
          ip == allowed
        end
      end
    end
  end

  # ==========================================================================
  # Response - HTTP Response Helpers
  # ==========================================================================
  module Response
    def self.text(message, status: 200)
      Protocol::HTTP::Response[
        status,
        { 'content-type' => 'text/plain; charset=UTF-8' },
        [message]
      ]
    end

    def self.json(data, status: 200)
      require 'json'
      Protocol::HTTP::Response[
        status,
        { 'content-type' => 'application/json; charset=UTF-8' },
        [JSON.generate(data)]
      ]
    end

    def self.html(content, status: 200)
      Protocol::HTTP::Response[
        status,
        { 'content-type' => 'text/html; charset=UTF-8' },
        [content]
      ]
    end

    def self.ok(message)           = text(message, status: 200)
    def self.forbidden(msg)        = text(msg, status: 403)
    def self.not_found(msg)        = text(msg, status: 404)
    def self.error(status_or_msg, msg = nil)
      if msg.nil?
        text(status_or_msg, status: 500)
      else
        text(msg, status: status_or_msg)
      end
    end

    # SSE Streaming Response — body is a Protocol::HTTP::Body::Writable
    def self.sse(body)
      Protocol::HTTP::Response[
        200,
        {
          'content-type'     => 'text/event-stream; charset=UTF-8',
          'cache-control'    => 'no-cache',
          'x-accel-buffering' => 'no'
        },
        body
      ]
    end
  end

  # ==========================================================================
  # Server - Main Server Class
  # ==========================================================================
  class Server
    def initialize(config)
      @config = config
      @stats = { started_at: Time.now, requests: 0, scripts_run: 0 }
    end

    def call(request)
      @stats[:requests] += 1
      client_ip = extract_ip(request)
      path = request.path

      # Security: IP allowlist check
      unless @config.allowed?(client_ip)
        log(:warn, "Blocked: #{client_ip} -> #{path}")
        return Response.forbidden("Access denied")
      end

      # Routing
      route(path, client_ip)
    end

    private

    # IP extraction (supports different async-http versions)
    def extract_ip(request)
      if request.respond_to?(:remote_address)
        addr = request.remote_address
        addr.respond_to?(:ip_address) ? addr.ip_address : addr.to_s
      else
        '0.0.0.0'
      end
    end

    # Request routing
    def route(path, client_ip)
      case path
      when '/', '/status'
        status_response
      when '/health'
        Response.ok('OK')
      when %r{^/list/?$}
        list_scripts
      when '/jobs/pending'
        Response.json(pending_jobs)
      when '/jobs', '/jobs/all'
        Response.json(all_jobs)
      when %r{^/jobs/ack/(.+)$}
        job_id = URI.decode_www_form_component(Regexp.last_match(1))
        acknowledge_job(job_id)
        Response.ok("acknowledged: #{job_id}")
      when '/notes'
        Response.json(list_note_sources)
      when %r{^/notes/([^/]+)/assets/(.+)$}
        source_id = Regexp.last_match(1)
        asset_path = Regexp.last_match(2)
        serve_note_asset(source_id, asset_path)
      when %r{^/notes/([^/]+)/(.+)$}
        source_id  = Regexp.last_match(1)
        filename   = Regexp.last_match(2)
        serve_note(source_id, filename)
      when %r{^/notes/([^/]+)/?$}
        source_id = Regexp.last_match(1)
        Response.json(list_notes(source_id))
      when %r{^/stream/}
        # Streaming execution: /stream/script_name/argument
        parts = path.split('/').reject(&:empty?)
        # parts[0] = 'stream', parts[1] = script, parts[2..] = args
        script_name = parts[1].to_s
        argument    = URI.decode_www_form_component(parts[2..].join('/'))
        stream_script(script_name, argument, client_ip)
      else
        # Script execution: /script_name/argument
        parts = path.split('/').reject(&:empty?)
        return Response.not_found("No script specified") if parts.empty?

        script_name = parts[0]
        # URL decode: %20 → space, %2F → /, etc.
        argument = URI.decode_www_form_component(parts[1..].join('/'))

        execute_script(script_name, argument, client_ip)
      end
    end

    # Status page
    def status_response
      uptime = Time.now - @stats[:started_at]
      scripts = list_available_scripts

      Response.json({
        service: 'milan',
        version: VERSION,
        uptime_seconds: uptime.round,
        requests: @stats[:requests],
        scripts_run: @stats[:scripts_run],
        available_scripts: scripts,
        scripts_dir: @config.scripts_dir
      })
    end

    # List available scripts
    def list_scripts
      scripts = list_available_scripts
      Response.json({ scripts: scripts })
    end

    def find_script(script_name)
      matches = SCRIPT_EXTENSIONS.map { |ext|
        File.join(@config.scripts_dir, "#{script_name}#{ext}")
      }.select { |f| File.exist?(f) }

      case matches.size
      when 0 then nil
      when 1 then matches.first
      else raise "Ambiguous script '#{script_name}': #{matches.map { |f| File.basename(f) }.join(', ')}"
      end
    end

    def list_available_scripts
      SCRIPT_EXTENSIONS.flat_map { |ext|
        Dir.glob(File.join(@config.scripts_dir, "*#{ext}"))
      }.map { |f| File.basename(f, File.extname(f)) }
       .sort.uniq
    end

    # Execute script (synchronous with timeout)
    def execute_script(script_name, argument, client_ip)
      # Security: only alphanumeric names + underscores/hyphens
      unless script_name.match?(/\A[\w-]+\z/)
        log(:warn, "Invalid script name: #{script_name}")
        return Response.forbidden("Invalid script name")
      end

      script_path = find_script(script_name)

      unless script_path
        log(:warn, "Script not found: #{script_name}")
        return Response.not_found("Script '#{script_name}' not found")
      end

      # Logging
      log(:info, "#{client_ip} -> #{script_name}(#{argument})")

      # Synchronous execution with timeout
      result = run_script(script_path, argument)
      @stats[:scripts_run] += 1

      if result[:success]
        log(:info, "#{script_name} completed (#{result[:duration]}ms)")
        Response.ok(result[:output])
      else
        log(:warn, "#{script_name} failed: #{result[:error]}")
        Response.text(result[:error], status: 422)
      end
    rescue => e
      log(:error, "Execution failed: #{e.message}")
      Response.error("Execution failed: #{e.message}")
    end

    # ── Notes (wiki/notebook) ─────────────────────────────────────────────────

    def note_sources
      sources = @config.notes || []
      sources.each_with_object({}) { |s, h| h[s['id']] = s['path'] }
    end

    def list_note_sources
      note_sources.map { |id, path| { 'id' => id, 'path' => path } }
    end

    def list_notes(source_id)
      dir = note_sources[source_id]
      return [] unless dir && File.directory?(dir)

      Dir[File.join(dir, '*.{md,html}')]
        .reject { |f| File.basename(f).start_with?('.') }
        .map    { |f| File.basename(f) }
        .sort_by(&:downcase)
    rescue
      []
    end

    def serve_note(source_id, filename)
      dir = note_sources[source_id]
      return Response.error(404, 'Source not found') unless dir

      # Sanitise filename — no path traversal
      name = File.basename(filename)
      return Response.error(400, 'Invalid filename') unless name.match?(/\A[\w. -]+\z/)

      path = File.join(dir, name)
      return Response.error(404, "Note '#{name}' not found") unless File.exist?(path)

      ext  = File.extname(name).downcase
      raw  = File.read(path, encoding: 'utf-8')

      html = case ext
             when '.md'
               # Strip Cheaters-style JSON frontmatter if present
               md = raw.include?('%%%END') ? raw.split('%%%END', 2).last.strip : raw
               stdout, = Open3.capture2('apex', '--mode', 'gfm', stdin_data: md)
               stdout
             when '.html'
               raw
             else
               return Response.error(415, 'Unsupported format')
             end

      Response.html(html)
    rescue => e
      Response.error(500, e.message)
    end

    def serve_note_asset(source_id, asset_path)
      dir = note_sources[source_id]
      return Response.error(404, 'Source not found') unless dir

      # Only allow images/ and css/ subdirectories, no traversal
      unless asset_path.match?(%r{\A(images|css)/[\w. -]+\z})
        return Response.error(403, 'Forbidden')
      end

      full_path = File.join(dir, asset_path)
      return Response.error(404, 'Asset not found') unless File.exist?(full_path)

      ext  = File.extname(full_path).downcase
      mime = case ext
             when '.png'  then 'image/png'
             when '.jpg', '.jpeg' then 'image/jpeg'
             when '.gif'  then 'image/gif'
             when '.svg'  then 'image/svg+xml'
             when '.webp' then 'image/webp'
             when '.css'  then 'text/css'
             else              'application/octet-stream'
             end

      data = File.binread(full_path)
      Protocol::HTTP::Response[200, { 'content-type' => mime }, [data]]
    rescue => e
      Response.error(500, e.message)
    end

    # ── Background job tracking ───────────────────────────────────────────────

    def write_job_log(job_id, lines)
      FileUtils.mkdir_p(JOBS_DIR)
      path = File.join(JOBS_DIR, "#{job_id}.log")
      File.write(path, lines.join("\n"))
      path
    end

    def record_job(job_id, script_name, exit_ok, log_path)
      status_file = File.join(JOBS_DIR, 'status.json')
      JOBS_MUTEX.synchronize do
        jobs = File.exist?(status_file) ? JSON.parse(File.read(status_file)) : []
        jobs << {
          'id'           => job_id,
          'script'       => script_name,
          'exit_ok'      => exit_ok,
          'log'          => log_path,
          'ts'           => Time.now.iso8601,
          'acknowledged' => false
        }
        jobs = jobs.last(100)   # keep history bounded
        atomic_write(status_file, JSON.generate(jobs))
      end
    end

    def all_jobs
      status_file = File.join(JOBS_DIR, 'status.json')
      return [] unless File.exist?(status_file)
      JSON.parse(File.read(status_file))
    rescue
      []
    end

    def pending_jobs
      all_jobs.reject { |j| j['acknowledged'] }
    end

    def acknowledge_job(job_id)
      status_file = File.join(JOBS_DIR, 'status.json')
      JOBS_MUTEX.synchronize do
        return unless File.exist?(status_file)
        jobs = JSON.parse(File.read(status_file))
        jobs.each { |j| j['acknowledged'] = true if j['id'] == job_id }
        atomic_write(status_file, JSON.generate(jobs))
      end
    end

    def atomic_write(path, content)
      tmp = "#{path}.tmp"
      File.write(tmp, content)
      File.rename(tmp, path)
    end

    # ── Script execution helpers ───────────────────────────────────────────────

    # Build command array for a script path + argument
    def build_cmd(script_path, argument)
      case File.extname(script_path)
      when '.rb' then [RbConfig.ruby, script_path, argument]
      when '.sh' then ['sh', script_path, argument]
      when '.py' then ['python3', script_path, argument]
      else [script_path, argument]
      end
    end

    # Stream script output as SSE (Server-Sent Events)
    # Returns immediately; output is written asynchronously line by line.
    def stream_script(script_name, argument, client_ip)
      unless script_name.match?(/\A[\w-]+\z/)
        log(:warn, "Invalid script name: #{script_name}")
        return Response.forbidden("Invalid script name")
      end

      script_path = find_script(script_name)
      unless script_path
        log(:warn, "Stream: script not found: #{script_name}")
        return Response.not_found("Script '#{script_name}' not found")
      end

      log(:info, "#{client_ip} ~> #{script_name}(#{argument}) [stream]")
      @stats[:scripts_run] += 1

      body = Protocol::HTTP::Body::Writable.new

      Async do
        silent    = false   # true once client disconnects
        log_lines = []
        job_id    = "#{script_name}_#{Time.now.strftime('%Y%m%d_%H%M%S')}"

        IO.popen(build_cmd(script_path, argument), err: [:child, :out]) do |io|
          io.each_line do |line|
            if silent
              log_lines << line.chomp   # collect for logfile, don't stream
              next
            end
            begin
              body.write("data: #{line.chomp}\n\n")
            rescue
              silent = true             # client gone → background mode
              log_lines << line.chomp
              log(:info, "#{script_name} → background (#{job_id})")
            end
          end
        end

        exit_ok = $?.success?

        if silent
          log_path = write_job_log(job_id, log_lines)
          record_job(job_id, script_name, exit_ok, log_path)
          log(:info, "#{script_name} background #{exit_ok ? 'ok' : 'failed'} → #{log_path}")
        else
          if exit_ok
            log(:info, "#{script_name} stream completed")
          else
            body.write("event: stream_error\ndata: exit #{$?.exitstatus}\n\n") rescue nil
            log(:warn, "#{script_name} stream exited #{$?.exitstatus}")
          end
          body.write("event: done\ndata: \n\n") rescue nil
          body.close
        end
      end

      Response.sse(body)
    end

    # Synchronous script execution with timeout (5s)
    def run_script(script_path, argument)
      start_time = Process.clock_gettime(Process::CLOCK_MONOTONIC)

      # IO.popen for stdout capture
      output = nil
      status = nil

      Timeout.timeout(5) do
        IO.popen(build_cmd(script_path, argument), err: [:child, :out]) do |io|
          output = io.read
        end
        status = $?
      end

      duration = ((Process.clock_gettime(Process::CLOCK_MONOTONIC) - start_time) * 1000).round

      if status&.success?
        { success: true, output: output.strip, duration: duration }
      else
        { success: false, error: output.strip.empty? ? "Exit code: #{status&.exitstatus}" : output.strip, duration: duration }
      end
    rescue Timeout::Error
      { success: false, error: "Timeout (>5s)", duration: 5000 }
    end

    # Logging
    def log(level, message)
      timestamp = Time.now.strftime('%H:%M:%S')
      prefix = case level
               when :warn  then "\e[33mWARN\e[0m"
               when :error then "\e[31mERROR\e[0m"
               else             "\e[32mINFO\e[0m"
               end
      puts "[#{timestamp}] #{prefix} #{message}"
    end
  end
end

# ============================================================================
# Main Execution
# ============================================================================
if __FILE__ == $PROGRAM_NAME
  puts <<~BANNER
    \e[36m
    ╔═══════════════════════════════════════╗
    ║          Milan v#{Milan::VERSION}               ║
    ║    Script Executor for macOS          ║
    ╚═══════════════════════════════════════╝
    \e[0m
  BANNER

  begin
    config = Milan::Config.new
    server = Milan::Server.new(config)

    cron_script = File.join(__dir__, 'scripts', 'cron-runner.rb')

    puts "Port:        #{config.port}"
    puts "Scripts:     #{config.scripts_dir}"
    puts "Allowed IPs: #{config.allowed_ips.join(', ')}"
    puts "Cron:        every #{config.cron_interval}s" if File.exist?(cron_script)
    puts "─" * 40
    puts "Endpoints:"
    puts "  GET /                    Status (JSON)"
    puts "  GET /health              Health Check"
    puts "  GET /list                Available Scripts"
    puts "  GET /<script>            Execute script"
    puts "  GET /<script>/<arg>      Execute with argument"
    puts "  GET /stream/<script>     Stream output (SSE)"
    puts "  GET /jobs/all            All background jobs"
    puts "  GET /jobs/pending        Unacknowledged jobs"
    puts "  GET /jobs/ack/<id>       Acknowledge job"
    puts "  GET /notes               List note sources (JSON)"
    puts "  GET /notes/<source>       List notes in source (JSON)"
    puts "  GET /notes/<source>/<file> Render note (HTML)"
    puts "  GET /notes/<source>/assets/<path> Serve asset"
    puts "─" * 40
    puts "Press Ctrl+C to stop\n\n"

    endpoint = Async::HTTP::Endpoint.parse("http://0.0.0.0:#{config.port}")

    Async do
      Async::HTTP::Server.for(endpoint) do |request|
        server.call(request)
      end.run

      if File.exist?(cron_script)
        Async do
          loop do
            sleep config.cron_interval
            ts = Time.now.strftime('%H:%M:%S')
            puts "[#{ts}] \e[32mINFO\e[0m cron tick"
            IO.popen([RbConfig.ruby, cron_script], err: [:child, :out]) { |io| io.read }
          end
        end
      end
    end

  rescue Interrupt
    puts "\n\e[33mMilan stopped.\e[0m"
    exit 0
  rescue => e
    puts "\e[31mFatal: #{e.message}\e[0m"
    puts e.backtrace.first(5).join("\n")
    exit 1
  end
end
