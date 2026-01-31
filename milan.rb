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
require 'yaml'
require 'fileutils'
require 'timeout'
require 'uri'

# Disable DNS reverse lookup (saves ~80-100ms per request)
require 'socket'
BasicSocket.do_not_reverse_lookup = true

module Milan
  VERSION = '1.0.0'

  # ==========================================================================
  # Config - Loads configuration from YAML
  # ==========================================================================
  class Config
    attr_reader :port, :allowed_ips, :scripts_dir

    def initialize(config_path = nil)
      config_path ||= File.join(__dir__, 'config.yaml')
      @config = YAML.load_file(config_path)

      milan_config = @config['milan'] || {}
      @port = milan_config['port'] || 8080
      @allowed_ips = Array(milan_config['allowed_ips'])
      @scripts_dir = milan_config['scripts_dir'] || './scripts'

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

    def self.ok(message)      = text(message, status: 200)
    def self.forbidden(msg)   = text(msg, status: 403)
    def self.not_found(msg)   = text(msg, status: 404)
    def self.error(msg)       = text(msg, status: 500)
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

    def list_available_scripts
      Dir.glob(File.join(@config.scripts_dir, '*.rb'))
         .map { |f| File.basename(f, '.rb') }
         .sort
    end

    # Execute script (synchronous with timeout)
    def execute_script(script_name, argument, client_ip)
      # Security: only alphanumeric names + underscores/hyphens
      unless script_name.match?(/\A[\w-]+\z/)
        log(:warn, "Invalid script name: #{script_name}")
        return Response.forbidden("Invalid script name")
      end

      script_path = File.join(@config.scripts_dir, "#{script_name}.rb")

      unless File.exist?(script_path)
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

    # Synchronous script execution with timeout (5s)
    def run_script(script_path, argument)
      start_time = Process.clock_gettime(Process::CLOCK_MONOTONIC)

      # IO.popen for stdout capture
      output = nil
      status = nil

      Timeout.timeout(5) do
        IO.popen([RbConfig.ruby, script_path, argument], err: [:child, :out]) do |io|
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

    puts "Port:        #{config.port}"
    puts "Scripts:     #{config.scripts_dir}"
    puts "Allowed IPs: #{config.allowed_ips.join(', ')}"
    puts "─" * 40
    puts "Endpoints:"
    puts "  GET /            Status (JSON)"
    puts "  GET /health      Health Check"
    puts "  GET /list        Available Scripts"
    puts "  GET /<script>    Execute script"
    puts "  GET /<script>/<arg>  Execute with argument"
    puts "─" * 40
    puts "Press Ctrl+C to stop\n\n"

    endpoint = Async::HTTP::Endpoint.parse("http://0.0.0.0:#{config.port}")

    Async do
      Async::HTTP::Server.for(endpoint) do |request|
        server.call(request)
      end.run
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
