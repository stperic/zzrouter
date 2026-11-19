import AppKit

// The desktop entry delegates installation and browser authorization to the
// shipped CLI. It owns presentation only, with no credential or policy storage.
final class RouterApplication: NSObject, NSApplicationDelegate {
    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.regular)
        NSApp.activate(ignoringOtherApps: true)
        let home = FileManager.default.homeDirectoryForCurrentUser
        let release = Bundle.main.resourceURL?.appendingPathComponent("release")
        let client = home.appendingPathComponent(".local/bin/zzrouter")
        let existing = FileManager.default.isExecutableFile(atPath: client.path)
        var chosenRole = "coordinator"
        if existing {
            do {
                guard let inspector = release?.appendingPathComponent("zzrouter-node") else {
                    throw NSError(domain: "Router", code: 1, userInfo: [NSLocalizedDescriptionKey: "The app has no installation bundle."])
                }
                let output = try run(inspector.path, ["config", "identity", "--json"], directory: home)
                guard let identity = try JSONSerialization.jsonObject(with: Data(output.utf8)) as? [String: Any],
                      let mode = identity["mode"] as? String else {
                    throw NSError(domain: "Router", code: 1, userInfo: [NSLocalizedDescriptionKey: "Could not read this installation’s role."])
                }
                chosenRole = mode
            } catch {
                let failure = NSAlert()
                failure.messageText = "Could not read your installation"
                failure.informativeText = error.localizedDescription
                failure.runModal()
                NSApp.terminate(nil)
                return
            }
        }
        let choice = NSAlert()
        choice.messageText = existing ? "Open your router" : "Set up your router"
        choice.informativeText = existing
            ? (chosenRole == "worker" ? "View connection instructions, or update this worker. Your models and settings are preserved." : "Open the console, or update this installation from the app. Your accounts, models and settings are preserved.")
            : "Install zzRouter for your account and start it when you log in. Choose whether to create a router or join one you already have."
        choice.addButton(withTitle: existing ? (chosenRole == "worker" ? "Connection instructions" : "Open console") : "Install and open")
        if existing { choice.addButton(withTitle: "Install or update") }
        choice.addButton(withTitle: "Cancel")
        let selection = choice.runModal()
        if selection == (existing ? .alertThirdButtonReturn : .alertSecondButtonReturn) {
            NSApp.terminate(nil)
            return
        }
        let install = !existing || selection == .alertSecondButtonReturn
        if !existing {
            let role = NSAlert()
            role.messageText = "How will you use this computer?"
            role.informativeText = "Create a router to manage your models and access. It can also run models here. Join an existing router to add this computer as a worker."
            role.addButton(withTitle: "Create a router")
            role.addButton(withTitle: "Join an existing router")
            role.addButton(withTitle: "Cancel")
            let selectedRole = role.runModal()
            if selectedRole == .alertThirdButtonReturn { NSApp.terminate(nil); return }
            if selectedRole == .alertSecondButtonReturn { chosenRole = "worker" }
        }
        let role = chosenRole
        var workerPort = "9091"
        if role == "worker" && !existing {
            let connection = NSAlert()
            connection.messageText = "Match your coordinator’s cluster port"
            connection.informativeText = "Open Nodes > Add compute node in your coordinator console. Enter the Worker TCP port shown under Connection details."
            let port = NSTextField(string: workerPort)
            port.frame = NSRect(x: 0, y: 0, width: 240, height: 24)
            port.setAccessibilityLabel("Worker TCP port")
            connection.accessoryView = port
            connection.addButton(withTitle: "Install worker")
            connection.addButton(withTitle: "Cancel")
            while true {
                if connection.runModal() == .alertSecondButtonReturn { NSApp.terminate(nil); return }
                if let value = Int(port.stringValue), (1...65535).contains(value) { workerPort = String(value); break }
                connection.informativeText = "Enter a port from 1 to 65535, matching your coordinator’s Worker TCP port."
            }
        }
        let clusterPort = workerPort
        let progress = NSPanel(contentRect: NSRect(x: 0, y: 0, width: 410, height: 100),
                               styleMask: [.titled], backing: .buffered, defer: false)
        progress.title = "Router"
        let label = NSTextField(labelWithString: install ? "Preparing your router…" : "Opening your router…")
        label.frame = NSRect(x: 24, y: 36, width: 360, height: 24)
        progress.contentView?.addSubview(label)
        progress.center()
        progress.makeKeyAndOrderFront(nil)
        DispatchQueue.global(qos: .userInitiated).async {
            do {
                if install {
                    guard let bundle = release else {
                        throw NSError(domain: "Router", code: 1, userInfo: [NSLocalizedDescriptionKey: "The app has no installation bundle."])
                    }
                    _ = try self.run("/bin/bash", [bundle.appendingPathComponent("install.sh").path, "--yes", "--role", role, "--cluster-port", clusterPort], directory: bundle)
                }
                let output = role == "worker"
                    ? "Open Nodes > Add compute node on your coordinator. Select macOS and copy its pairing command. Run that command in Terminal on this computer, then paste the pairing details back into the console for approval."
                    : try self.run(client.path, ["console"], directory: home)
                DispatchQueue.main.async {
                    progress.close()
                    let ready = NSAlert()
                    ready.messageText = role == "worker" ? "Connect to your existing router" : "Continue in your browser"
                    ready.informativeText = role == "worker"
                        ? "This worker starts when you log in. Your coordinator will guide you through the connection."
                        : "Paste the temporary code below into Use a temporary code in the console. It expires shortly and can be used only once."
                    let text = NSTextView(frame: NSRect(x: 0, y: 0, width: 480, height: 145))
                    text.isEditable = false
                    text.isSelectable = true
                    text.font = NSFont.monospacedSystemFont(ofSize: 12, weight: .regular)
                    text.string = output
                    ready.accessoryView = text
                    ready.addButton(withTitle: "Done")
                    ready.runModal()
                    NSApp.terminate(nil)
                }
            } catch {
                DispatchQueue.main.async {
                    progress.close()
                    let failure = NSAlert()
                    failure.messageText = "Your router needs attention"
                    failure.informativeText = error.localizedDescription
                    failure.addButton(withTitle: "Close")
                    failure.runModal()
                    NSApp.terminate(nil)
                }
            }
        }
    }

    private func run(_ executable: String, _ arguments: [String], directory: URL) throws -> String {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: executable)
        process.arguments = arguments
        process.currentDirectoryURL = directory
        let pipe = Pipe()
        process.standardOutput = pipe
        process.standardError = pipe
        try process.run()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        let output = String(decoding: data, as: UTF8.self)
        if process.terminationStatus != 0 {
            throw NSError(domain: "Router", code: Int(process.terminationStatus),
                          userInfo: [NSLocalizedDescriptionKey: String(output.suffix(4000))])
        }
        return output
    }
}

let app = NSApplication.shared
let delegate = RouterApplication()
app.delegate = delegate
app.run()
