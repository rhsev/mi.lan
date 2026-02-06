import AppKit

// MilanOpener — URL scheme handler for milan://
//
// Registers milan:// and forwards to the local Milan agent.
// URL format: milan://script/argument
// Example:    milan://mail/dfc8d29c-37d6-ee1c-9131-0adb73324487@domain-contact.org
//             → calls http://localhost:8080/mail/dfc8d29c-...
//
// The app runs as LSBackgroundOnly — no dock icon, no window.

class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationDidFinishLaunching(_ notification: Notification) {
        // Register for URL events
        NSAppleEventManager.shared().setEventHandler(
            self,
            andSelector: #selector(handleURL(_:withReply:)),
            forEventClass: AEEventClass(kInternetEventClass),
            andEventID: AEEventID(kAEGetURL)
        )
    }

    @objc func handleURL(_ event: NSAppleEventDescriptor, withReply reply: NSAppleEventDescriptor) {
        guard let urlString = event.paramDescriptor(forKeyword: AEKeyword(keyDirectObject))?.stringValue,
              let url = URL(string: urlString) else {
            NSLog("MilanOpener: Invalid URL")
            NSApplication.shared.terminate(nil)
            return
        }

        // milan://mail/abc123 → host: "mail", path: "/abc123"
        // Reconstruct as: /mail/abc123
        let script = url.host ?? ""
        let argument = url.path  // includes leading /

        let milanPath = "/\(script)\(argument)"
        let milanURL = "http://localhost:8080\(milanPath)"

        NSLog("MilanOpener: \(urlString) → \(milanURL)")

        // Fire and forget — call Milan asynchronously
        guard let requestURL = URL(string: milanURL) else {
            NSLog("MilanOpener: Invalid Milan URL")
            NSApplication.shared.terminate(nil)
            return
        }

        let task = URLSession.shared.dataTask(with: requestURL) { _, _, error in
            if let error = error {
                NSLog("MilanOpener: Error — \(error.localizedDescription)")
            }
            // Quit after request completes
            DispatchQueue.main.async {
                NSApplication.shared.terminate(nil)
            }
        }
        task.resume()

        // Timeout: quit after 10 seconds even if no response
        DispatchQueue.main.asyncAfter(deadline: .now() + 10) {
            NSApplication.shared.terminate(nil)
        }
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.run()
