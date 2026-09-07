import Foundation

struct RoomScenario: Identifiable {
    let id: String
    let note: String
    let instructions: String
    let script: [String]
    let tools: [[String: Any]]

    static func load() -> [RoomScenario] {
        guard let url = Bundle.module.url(forResource: "room-scenarios", withExtension: "json", subdirectory: "Resources"),
              let data = try? Data(contentsOf: url),
              let rows = (try? JSONSerialization.jsonObject(with: data)) as? [[String: Any]] else { return [] }
        return rows.compactMap { row in
            guard let name = row["name"] as? String, let instructions = row["instructions"] as? String else { return nil }
            let lines = row["script"] as? [[String: Any]] ?? []
            return RoomScenario(id: name, note: row["note"] as? String ?? "", instructions: instructions,
                script: lines.map { "\($0["Speaker"] as? String ?? "user"): \($0["Text"] as? String ?? "")" },
                tools: row["tools"] as? [[String: Any]] ?? [])
        }
    }
}
