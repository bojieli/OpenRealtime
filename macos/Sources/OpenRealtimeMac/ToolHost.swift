import Foundation

actor LocalToolHost {
    typealias Confirm = @Sendable (String, String, [String: Any]) async -> Bool
    typealias ComputerAction = @Sendable (String, [String: Any]) async throws -> String

    let root: URL
    let declarations: [[String: Any]]

    private let confirm: Confirm
    private let computerAction: ComputerAction
    private let mode: ComputerMode
    private var artifactVersions: [String: Int] = [:]
    private var downloadVersions: [String: Int] = [:]
    private var downloads: [String: DownloadRecord] = [:]
    private let maxTextBytes = 1 << 20
    private let maxOutputBytes = 64 << 10
    private let maxDownloadBytes = 8 << 20

    init(rootPath: String, mode: ComputerMode, width: Int, height: Int,
         confirm: @escaping Confirm, computerAction: @escaping ComputerAction) throws {
        let candidate = URL(fileURLWithPath: rootPath, isDirectory: true)
            .standardizedFileURL.resolvingSymlinksInPath()
        var isDirectory: ObjCBool = false
        guard FileManager.default.fileExists(atPath: candidate.path, isDirectory: &isDirectory), isDirectory.boolValue else {
            throw ToolHostError("the workspace root is not an existing directory")
        }
        self.root = candidate
        self.mode = mode
        self.confirm = confirm
        self.computerAction = computerAction
        self.declarations = Self.makeDeclarations(mode: mode, width: width, height: height)
    }

    func execute(name: String, arguments: [String: Any]) async -> Result<ToolExecutionResult, Error> {
        do {
            let result: ToolExecutionResult
            switch name {
            case "read_file": result = try readFile(arguments)
            case "list_directory": result = try listDirectory(arguments)
            case "search_files": result = try searchFiles(arguments)
            case "write_file": result = try await writeFile(arguments)
            case "run_command": result = try await runCommand(arguments)
            case "display_artifact": result = try displayArtifact(arguments)
            case "publish_download": result = try publishDownload(arguments)
            case let action where action.hasPrefix("computer."):
                result = try await runComputer(action, arguments)
            default: throw ToolHostError("no client tool named \"\(name)\" is declared")
            }
            return .success(result)
        } catch {
            return .failure(error)
        }
    }

    func declarationsFor(width: Int, height: Int) -> [[String: Any]] {
        Self.makeDeclarations(mode: mode, width: width, height: height)
    }

    private func readFile(_ arguments: [String: Any]) throws -> ToolExecutionResult {
        let url = try resolve(requiredString("path", arguments))
        let values = try url.resourceValues(forKeys: [.isRegularFileKey, .fileSizeKey])
        guard values.isRegularFile == true else { throw ToolHostError("the requested path is not a regular file") }
        guard (values.fileSize ?? 0) <= maxTextBytes else { throw ToolHostError("the file exceeds the 1 MiB text limit") }
        let data = try Data(contentsOf: url)
        guard let text = String(data: data, encoding: .utf8) else { throw ToolHostError("the file is not UTF-8 text") }
        return ToolExecutionResult(output: text)
    }

    private func listDirectory(_ arguments: [String: Any]) throws -> ToolExecutionResult {
        let url = try resolve((arguments["path"] as? String) ?? "")
        let entries = try FileManager.default.contentsOfDirectory(
            at: url, includingPropertiesForKeys: [.isDirectoryKey], options: []
        ).sorted { $0.lastPathComponent.localizedStandardCompare($1.lastPathComponent) == .orderedAscending }
        let lines = try entries.map { entry -> String in
            let directory = try entry.resourceValues(forKeys: [.isDirectoryKey]).isDirectory == true
            return entry.lastPathComponent + (directory ? "/" : "")
        }
        return ToolExecutionResult(output: lines.isEmpty ? "directory is empty" : lines.joined(separator: "\n"))
    }

    private func searchFiles(_ arguments: [String: Any]) throws -> ToolExecutionResult {
        let pattern = try requiredString("pattern", arguments)
        let expression = try NSRegularExpression(pattern: pattern)
        let searchRoot = try resolve((arguments["path"] as? String) ?? "")
        let maximum = max(1, min(500, (arguments["max_results"] as? Int) ?? 50))
        guard let enumerator = FileManager.default.enumerator(
            at: searchRoot,
            includingPropertiesForKeys: [.isDirectoryKey, .isRegularFileKey, .fileSizeKey],
            options: [], errorHandler: { _, _ in true }
        ) else { throw ToolHostError("the search path could not be enumerated") }
        var matches: [String] = []
        while let url = enumerator.nextObject() as? URL, matches.count < maximum {
            let name = url.lastPathComponent
            if [".git", "node_modules", "vendor", ".runtime", "__pycache__"].contains(name) {
                enumerator.skipDescendants()
                continue
            }
            // An enumerated symlink can still point outside the root. Resolve
            // every file again at the point of read; the enumerator's lexical
            // location is not an authority boundary.
            guard let safeURL = try? resolve(url.path) else { continue }
            let values = try? safeURL.resourceValues(forKeys: [.isDirectoryKey, .isRegularFileKey, .fileSizeKey])
            guard values?.isRegularFile == true, (values?.fileSize ?? maxTextBytes + 1) <= maxTextBytes,
                  let data = try? Data(contentsOf: safeURL), let text = String(data: data, encoding: .utf8) else { continue }
            for (index, line) in text.components(separatedBy: .newlines).enumerated() {
                let range = NSRange(line.startIndex..<line.endIndex, in: line)
                if expression.firstMatch(in: line, range: range) != nil {
                    matches.append("\(relative(safeURL)):\(index + 1): \(line.trimmingCharacters(in: .whitespaces))")
                    if matches.count == maximum { break }
                }
            }
        }
        return ToolExecutionResult(output: matches.isEmpty ? "no matches for \(pattern)" : matches.joined(separator: "\n"))
    }

    private func writeFile(_ arguments: [String: Any]) async throws -> ToolExecutionResult {
        let path = try requiredString("path", arguments)
        let content = try requiredString("content", arguments, allowEmpty: true)
        guard content.utf8.count <= maxTextBytes else { throw ToolHostError("write_file is limited to 1 MiB") }
        let url = try resolve(path, allowMissing: true)
        let allowed = await confirm("write_file", "This creates or replaces a file inside the selected workspace.", arguments)
        guard allowed else { throw ToolHostError("write_file was declined locally") }
        guard FileManager.default.fileExists(atPath: url.deletingLastPathComponent().path) else {
            throw ToolHostError("the destination directory does not exist")
        }
        try Data(content.utf8).write(to: url, options: .atomic)
        return ToolExecutionResult(output: "wrote \(content.utf8.count) bytes to \(relative(url))")
    }

    private func runCommand(_ arguments: [String: Any]) async throws -> ToolExecutionResult {
        let command = try requiredString("command", arguments)
        let allowed = await confirm("run_command", "This executes a shell command with the selected workspace as its working directory.", arguments)
        guard allowed else { throw ToolHostError("run_command was declined locally") }

        let process = Process()
        let pipe = Pipe()
        process.executableURL = URL(fileURLWithPath: "/bin/zsh")
        process.arguments = ["-lc", command]
        process.currentDirectoryURL = root
        process.standardOutput = pipe
        process.standardError = pipe

        let lock = NSLock()
        var output = Data()
        pipe.fileHandleForReading.readabilityHandler = { handle in
            let chunk = handle.availableData
            guard !chunk.isEmpty else { return }
            lock.lock()
            if output.count < (1 << 20) { output.append(chunk.prefix((1 << 20) - output.count)) }
            lock.unlock()
        }
        try process.run()
        let timeoutLock = NSLock()
        var timedOut = false
        DispatchQueue.global().asyncAfter(deadline: .now() + 120) {
            if process.isRunning {
                timeoutLock.lock(); timedOut = true; timeoutLock.unlock()
                process.terminate()
            }
        }
        process.waitUntilExit()
        pipe.fileHandleForReading.readabilityHandler = nil
        if let tail = try? pipe.fileHandleForReading.readToEnd() {
            lock.lock(); output.append(tail.prefix(max(0, (1 << 20) - output.count))); lock.unlock()
        }
        timeoutLock.lock(); let didTimeOut = timedOut; timeoutLock.unlock()
        if didTimeOut { throw ToolHostError("the command exceeded the 120 second limit") }
        let wasTruncated = output.count > maxOutputBytes
        let bounded = String(decoding: output.prefix(maxOutputBytes), as: UTF8.self)
            + (wasTruncated ? "\n[truncated at \(maxOutputBytes) bytes]" : "")
        let header = "exit \(process.terminationStatus)"
        return ToolExecutionResult(output: bounded.isEmpty ? header : "\(header)\n\(bounded)")
    }

    private func displayArtifact(_ arguments: [String: Any]) throws -> ToolExecutionResult {
        let id = try validID(requiredString("artifact_id", arguments))
        let title = try requiredString("title", arguments)
        let html = try requiredString("html", arguments)
        guard html.utf8.count <= 512 << 10 else { throw ToolHostError("the artifact exceeds 512 KiB") }
        let version = (artifactVersions[id] ?? 0) + 1
        artifactVersions[id] = version
        let record = ArtifactRecord(id: id, title: title, html: isolatedArtifactHTML(html), version: version)
        return ToolExecutionResult(
            output: compactJSONString(["artifact_id": id, "version": version, "status": "displayed"]),
            artifact: record
        )
    }

    private func publishDownload(_ arguments: [String: Any]) throws -> ToolExecutionResult {
        let id = try validID(requiredString("artifact_id", arguments))
        let filename = try safeFilename(requiredString("filename", arguments))
        let mediaType = try requiredString("media_type", arguments)
        guard mediaType.contains("/") else { throw ToolHostError("media_type must be an IANA type such as text/csv") }
        let hasText = arguments["text"] is String
        let hasBase64 = arguments["base64"] is String
        guard hasText != hasBase64 else { throw ToolHostError("publish_download requires exactly one of text or base64") }
        let data: Data
        if let text = arguments["text"] as? String { data = Data(text.utf8) }
        else if let encoded = arguments["base64"] as? String, let decoded = Data(base64Encoded: encoded) { data = decoded }
        else { throw ToolHostError("download base64 is invalid") }
        guard !data.isEmpty, data.count <= maxDownloadBytes else {
            throw ToolHostError("generated files must be 1 byte through \(maxDownloadBytes) bytes")
        }
        let base = try FileManager.default.url(
            for: .applicationSupportDirectory, in: .userDomainMask,
            appropriateFor: nil, create: true
        ).appendingPathComponent("OpenRealtime/Downloads", isDirectory: true)
        try FileManager.default.createDirectory(at: base, withIntermediateDirectories: true)
        let url = base.appendingPathComponent("\(id)-\(filename)", isDirectory: false)
        if let old = downloads[id], old.url != url { try? FileManager.default.removeItem(at: old.url) }
        try data.write(to: url, options: .atomic)
        let version = (downloadVersions[id] ?? 0) + 1
        downloadVersions[id] = version
        let record = DownloadRecord(
            id: id, filename: filename, mediaType: mediaType,
            bytes: data.count, version: version, url: url
        )
        downloads[id] = record
        return ToolExecutionResult(
            output: compactJSONString([
                "artifact_id": id, "filename": filename, "bytes": data.count,
                "version": version, "status": "available",
            ]),
            download: record
        )
    }

    private func runComputer(_ name: String, _ arguments: [String: Any]) async throws -> ToolExecutionResult {
        let known = Set(Self.computerNames(mode: mode))
        guard known.contains(name) else { throw ToolHostError("\(name) is not declared in \(mode.rawValue) mode") }
        if ["computer.click", "computer.click_element", "computer.double_click", "computer.drag",
            "computer.type", "computer.key"].contains(name) {
            let allowed = await confirm(name, "This action changes the explicitly selected \(mode.rawValue) target.", arguments)
            guard allowed else { throw ToolHostError("\(name) was declined locally") }
        }
        let output = try await computerAction(name, arguments)
        return ToolExecutionResult(output: output)
    }

    private func requiredString(_ key: String, _ arguments: [String: Any], allowEmpty: Bool = false) throws -> String {
        guard let value = arguments[key] as? String,
              allowEmpty || !value.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            throw ToolHostError("\(key) must be a \(allowEmpty ? "string" : "non-empty string")")
        }
        return value
    }

    private func resolve(_ path: String, allowMissing: Bool = false) throws -> URL {
        let requested = path.trimmingCharacters(in: .whitespacesAndNewlines)
        var candidate = requested.hasPrefix("/")
            ? URL(fileURLWithPath: requested)
            : root.appendingPathComponent(requested)
        candidate = candidate.standardizedFileURL

        var probe = candidate
        var suffix: [String] = []
        while !FileManager.default.fileExists(atPath: probe.path) {
            guard allowMissing || probe != candidate else { throw ToolHostError("path \(path) does not exist") }
            guard probe.path != "/" else { throw ToolHostError("path \(path) cannot be resolved") }
            suffix.insert(probe.lastPathComponent, at: 0)
            probe.deleteLastPathComponent()
        }
        var resolved = probe.resolvingSymlinksInPath().standardizedFileURL
        for component in suffix { resolved.appendPathComponent(component) }
        resolved = resolved.standardizedFileURL
        let rootPath = root.path.hasSuffix("/") ? root.path : root.path + "/"
        guard resolved.path == root.path || resolved.path.hasPrefix(rootPath) else {
            throw ToolHostError("path \(path) resolves outside the selected workspace")
        }
        return resolved
    }

    private func relative(_ url: URL) -> String {
        url.path == root.path ? "." : String(url.path.dropFirst(root.path.count + 1))
    }

    private func validID(_ id: String) throws -> String {
        guard id.range(of: #"^[A-Za-z0-9_-]{1,64}$"#, options: .regularExpression) != nil else {
            throw ToolHostError("artifact_id must contain 1–64 letters, digits, dashes, or underscores")
        }
        return id
    }

    private func safeFilename(_ filename: String) throws -> String {
        guard !filename.isEmpty, filename == URL(fileURLWithPath: filename).lastPathComponent,
              !filename.contains("/"), !filename.contains("\\"), !filename.contains("\n"), !filename.contains("\r") else {
            throw ToolHostError("filename must be one safe file name without a path")
        }
        return filename
    }

    private func isolatedArtifactHTML(_ html: String) -> String {
        let policy = #"<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data: blob:; connect-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'">"#
        if let head = html.range(of: #"<head(?:\s[^>]*)?>"#, options: [.regularExpression, .caseInsensitive]) {
            var document = html
            document.insert(contentsOf: policy, at: head.upperBound)
            return document
        }
        if let root = html.range(of: #"<html(?:\s[^>]*)?>"#, options: [.regularExpression, .caseInsensitive]) {
            var document = html
            document.insert(contentsOf: "<head>\(policy)</head>", at: root.upperBound)
            return document
        }
        return "<!doctype html><html><head><meta charset=\"utf-8\">\(policy)</head><body>\(html)</body></html>"
    }

    private static func makeDeclarations(mode: ComputerMode, width: Int, height: Int) -> [[String: Any]] {
        var result: [[String: Any]] = [
            tool("read_file", "Read a UTF-8 text file inside the selected workspace.", [
                "path": string("Path relative to the workspace root.")
            ], required: ["path"]),
            tool("list_directory", "List a directory inside the selected workspace.", [
                "path": string("Directory path; empty means the workspace root.")
            ]),
            tool("search_files", "Search UTF-8 files inside the selected workspace with a regular expression.", [
                "pattern": string("Regular expression."), "path": string("Directory path."),
                "max_results": ["type": "integer", "minimum": 1, "maximum": 500],
            ], required: ["pattern"]),
            tool("write_file", "Create or replace a UTF-8 file inside the selected workspace after local confirmation.", [
                "path": string("Destination relative to the workspace."), "content": string("Complete UTF-8 contents.")
            ], required: ["path", "content"]),
            tool("run_command", "Run a zsh command in the selected workspace after local confirmation.", [
                "command": string("Shell command line; execution is limited to 120 seconds.")
            ], required: ["command"]),
            tool("display_artifact", "Display or revise an isolated HTML artifact in the native app.", [
                "artifact_id": string("Stable id of letters, digits, dash, or underscore."),
                "title": string("Short visible title."), "html": string("Complete HTML document or fragment, up to 512 KiB."),
            ], required: ["artifact_id", "title", "html"]),
            tool("publish_download", "Publish or revise a generated file for the person to open, save, or reveal in Finder.", [
                "artifact_id": string("Stable id."), "filename": string("Safe file name without a path."),
                "media_type": string("IANA media type."), "text": string("UTF-8 contents."),
                "base64": string("Base64 binary contents; use instead of text."),
            ], required: ["artifact_id", "filename", "media_type"],
                 oneOf: [["required": ["text"]], ["required": ["base64"]]]),
        ]
        result.append(contentsOf: computerDeclarations(mode: mode, width: width, height: height))
        return result
    }

    private static func tool(_ name: String, _ description: String, _ properties: [String: Any],
                             required: [String] = [], target: String? = nil,
                             oneOf: [[String: Any]] = []) -> [String: Any] {
        var extensionObject: [String: Any] = ["confirm": "never"]
        if let target { extensionObject["target"] = target }
        var parameters: [String: Any] = [
            "type": "object", "properties": properties,
            "required": required, "additionalProperties": false,
        ]
        if !oneOf.isEmpty { parameters["oneOf"] = oneOf }
        return [
            "type": "function", "name": name, "description": description,
            "parameters": parameters,
            "openrealtime": extensionObject,
        ]
    }

    private static func string(_ description: String) -> [String: Any] {
        ["type": "string", "description": description]
    }

    private static func computerNames(mode: ComputerMode) -> [String] {
        var names = ["computer.click", "computer.double_click", "computer.move", "computer.drag",
                     "computer.type", "computer.key", "computer.scroll", "computer.screenshot", "computer.wait"]
        if mode == .browser { names.insert("computer.click_element", at: 1) }
        return names
    }

    private static func computerDeclarations(mode: ComputerMode, width: Int, height: Int) -> [[String: Any]] {
        let source: [String: Any] = ["type": "string", "enum": [mode.source],
                                     "description": "Explicit video source owned by this bounded target."]
        func coordinate(_ axis: String) -> [String: Any] {
            ["type": "integer", "minimum": 0, "maximum": max(0, (axis == "x" ? width : height) - 1)]
        }
        var definitions: [[String: Any]] = [
            tool("computer.click", "Click a pixel in the current bounded frame.", [
                "source": source, "x": coordinate("x"), "y": coordinate("y"),
                "button": ["type": "string", "enum": ["left", "right", "middle"]],
            ], required: ["source", "x", "y"], target: mode.target),
            tool("computer.double_click", "Double-click a pixel in the current bounded frame.", [
                "source": source, "x": coordinate("x"), "y": coordinate("y"),
            ], required: ["source", "x", "y"], target: mode.target),
            tool("computer.move", "Move the pointer without clicking.", [
                "source": source, "x": coordinate("x"), "y": coordinate("y"),
            ], required: ["source", "x", "y"], target: mode.target),
            tool("computer.drag", "Drag between two points in the current frame.", [
                "source": source, "from_x": coordinate("x"), "from_y": coordinate("y"),
                "to_x": coordinate("x"), "to_y": coordinate("y"),
            ], required: ["source", "from_x", "from_y", "to_x", "to_y"], target: mode.target),
            tool("computer.type", "Type literal text into the focused control.", [
                "source": source, "text": string("Literal text."),
                "element_id": string("Optional current set-of-mark id in Browser mode."),
            ], required: ["source", "text"], target: mode.target),
            tool("computer.key", "Press a key combination.", [
                "source": source, "keys": ["type": "array", "items": ["type": "string"], "minItems": 1],
            ], required: ["source", "keys"], target: mode.target),
            tool("computer.scroll", "Scroll at a point in the current frame.", [
                "source": source, "x": coordinate("x"), "y": coordinate("y"),
                "delta_x": ["type": "integer"], "delta_y": ["type": "integer"],
            ], required: ["source", "x", "y"], target: mode.target),
            tool("computer.screenshot", "Request a fresh frame from the bounded target.", ["source": source],
                 required: ["source"], target: mode.target),
            tool("computer.wait", "Wait briefly for the target to settle.", [
                "duration_ms": ["type": "integer", "minimum": 0, "maximum": 10_000],
            ], required: ["duration_ms"], target: mode.target),
        ]
        if mode == .browser {
            definitions.insert(tool(
                "computer.click_element", "Click the exact visible browser-use set-of-mark label in the current frame.",
                ["source": source, "element_id": string("Visible mark label exactly as shown.")],
                required: ["source", "element_id"], target: mode.target
            ), at: 1)
        }
        return definitions
    }
}

private struct ToolHostError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}
