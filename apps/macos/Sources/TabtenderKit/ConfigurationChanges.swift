import Foundation

extension Configuration {
    /// A JSON object holding only the settings that differ from `base`.
    ///
    /// `PUT /mgmt/v1/config` merges its body into the stored document, so
    /// sending just the edits leaves settings changed by someone else in the
    /// meantime alone.
    public func changes(from base: Configuration) throws -> Data {
        let changes = Self.difference(try jsonObject(), try base.jsonObject())
        return try JSONSerialization.data(withJSONObject: changes, options: [.sortedKeys])
    }

    /// Replays the edits made to `oldBase` on top of `newBase`.
    public func rebased(from oldBase: Configuration, onto newBase: Configuration) throws -> Configuration {
        let changes = Self.difference(try jsonObject(), try oldBase.jsonObject())
        let merged = Self.merge(changes, into: try newBase.jsonObject())
        return try JSONDecoder().decode(Configuration.self, from: JSONSerialization.data(withJSONObject: merged))
    }

    private func jsonObject() throws -> [String: Any] {
        try JSONSerialization.jsonObject(with: JSONEncoder().encode(self)) as? [String: Any] ?? [:]
    }

    /// The entries of `new` that differ from `old`, descending into objects.
    private static func difference(_ new: [String: Any], _ old: [String: Any]) -> [String: Any] {
        var changes: [String: Any] = [:]
        for (key, value) in new {
            if let nested = value as? [String: Any], let oldNested = old[key] as? [String: Any] {
                let nestedChanges = difference(nested, oldNested)
                if !nestedChanges.isEmpty {
                    changes[key] = nestedChanges
                }
            } else if !(value as AnyObject).isEqual(old[key]) {
                changes[key] = value
            }
        }
        return changes
    }

    private static func merge(_ changes: [String: Any], into base: [String: Any]) -> [String: Any] {
        var merged = base
        for (key, value) in changes {
            if let nested = value as? [String: Any], let baseNested = base[key] as? [String: Any] {
                merged[key] = merge(nested, into: baseNested)
            } else {
                merged[key] = value
            }
        }
        return merged
    }
}
