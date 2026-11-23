import Darwin
import Foundation

// A stand-in for a run's descendant that left its group: its own process group, alive while its parent is.
// With "child", it first starts another of itself and prints that child's pid, so a test can see a grandchild.
@main struct GroupLeader {
    static func main() { run() }
}

func run() {
if CommandLine.arguments.contains("child") {
    let task = Process()
    task.executableURL = URL(fileURLWithPath: CommandLine.arguments[0])
    try? task.run()
    if task.processIdentifier > 0 { print(task.processIdentifier); fflush(stdout) }
}
while getppid() > 1 { usleep(100_000) }
}
