import Foundation

private let slackSpans = try! NSRegularExpression(pattern:
    #"```[\s\S]*?```|`[^`\r\n]*`|<[^<>\r\n]+>|(?<![\p{L}\p{N}])([*_~])([^*_~`<>\r\n]+)\1(?![\p{L}\p{N}])"#)

/// Plain, noninteractive Slack preview. Slack resolves mentions, dates and custom emoji at display time; keep
/// their ids/codes visible rather than trusting a label supplied by the proposer. The verbatim source is shown
/// separately and is the only text sent. The preview never includes a live link or fetches content.
public func slackPlainPreview(_ text: String) -> String {
    func entities(_ text: String) -> String {
        // Slack decodes these three characters, exactly once.
        text.replacingOccurrences(of: "&lt;", with: "<").replacingOccurrences(of: "&gt;", with: ">")
            .replacingOccurrences(of: "&amp;", with: "&")
    }
    var output = ""
    var end = text.startIndex
    for match in slackSpans.matches(in: text, range: NSRange(text.startIndex..., in: text)) {
        guard let range = Range(match.range, in: text) else { continue }
        output += entities(String(text[end..<range.lowerBound]))
        let span = text[range]
        if span.hasPrefix("```") {
            output += entities(String(span.dropFirst(3).dropLast(3)))
        } else if span.hasPrefix("`") {
            output += entities(String(span.dropFirst().dropLast()))
        } else if span.hasPrefix("<") {
            let parts = span.dropFirst().dropLast().split(separator: "|", maxSplits: 1, omittingEmptySubsequences: false)
            let target = parts[0]
            if ["!here", "!channel", "!everyone"].contains(target) {
                output += "@" + target.dropFirst()
            } else if target.hasPrefix("@") || target.hasPrefix("#") {
                output += target
            } else if target.hasPrefix("https://") || target.hasPrefix("http://") || target.hasPrefix("mailto:") {
                output += entities(parts.count == 2 ? "\(parts[1]) (\(target))" : String(target))
            } else {
                output += entities(String(span))
            }
        } else if let inner = Range(match.range(at: 2), in: text),
                  text[inner].first?.isWhitespace == false, text[inner].last?.isWhitespace == false {
            output += entities(String(text[inner]))
        } else {
            output += entities(String(span))
        }
        end = range.upperBound
    }
    output += entities(String(text[end...]))
    return output
}
