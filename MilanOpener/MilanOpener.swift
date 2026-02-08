import AppKit

// MilanOpener — URL scheme handler for milan:// and ref://
//
// Registers milan:// and ref:// and forwards to the local Milan agent.
// URL format: milan://script/argument  or  ref://mail/id
// Example:    milan://mail/dfc8d29c  →  http://localhost:8080/mail/dfc8d29c
//
// The app runs as LSBackgroundOnly — no dock icon, no window.

class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationDidFinishLaunching(_ notification: Notification) {
        // Handler already registered before app.run()
    }

    @objc func handleURL(_ event: NSAppleEventDescriptor, withReply reply: NSAppleEventDescriptor) {
        guard let urlString = event.paramDescriptor(forKeyword: AEKeyword(keyDirectObject))?.stringValue,
              let url = URL(string: urlString) else {
            NSLog("MilanOpener: Invalid URL")
            NSApplication.shared.terminate(nil)
            return
        }

        let script = url.host ?? ""
        let argument = url.path

        let milanPath = "/\(script)\(argument)"
        let milanURL = "http://localhost:8080\(milanPath)"

        NSLog("MilanOpener: \(urlString) → \(milanURL)")

        guard let requestURL = URL(string: milanURL) else {
            NSLog("MilanOpener: Invalid Milan URL")
            NSApplication.shared.terminate(nil)
            return
        }

        let task = URLSession.shared.dataTask(with: requestURL) { _, _, error in
            if let error = error {
                NSLog("MilanOpener: Error — \(error.localizedDescription)")
            }
            DispatchQueue.main.async {
                NSApplication.shared.terminate(nil)
            }
        }
        task.resume()

        DispatchQueue.main.asyncAfter(deadline: .now() + 10) {
            NSApplication.shared.terminate(nil)
        }
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate

// Register URL handler BEFORE app.run() to catch the initial launch event
NSAppleEventManager.shared().setEventHandler(
    delegate,
    andSelector: #selector(AppDelegate.handleURL(_:withReply:)),
    forEventClass: AEEventClass(kInternetEventClass),
    andEventID: AEEventID(kAEGetURL)
)

app.run()
