// The macOS capture helper.
//
// Modes match native/windows/capture.cpp, because the orchestrator is shared
// and does not know which platform it is driving:
//
//   --preflight        report whether both tracks can be captured, as JSON
//   --list-apps        list processes producing audio, as JSON
//   --duration-ms N    stop after N milliseconds (default: until stdin closes)
//   --mic-only         capture track 0 only
//   --system-only      capture track 1 only
//   --app-pid N        capture only that process rather than the whole machine
//
// Stopping is closing stdin. Nothing else is listened for: a terminal Ctrl-C
// signals the whole foreground process group, and a helper that dies from it
// reports a perfectly good recording as failed. The orchestrator starts this in
// its own process group and closes stdin to stop it, which is the only route
// that lets the packet in hand finish and the END frames be written.

import Foundation
import CoreAudio

// --- JSON, written by hand -------------------------------------------------
//
// The reports are small and fixed, and hand-writing them keeps the field names
// visibly identical to the Go structs that parse them.

func jsonString(_ s: String) -> String {
    var out = "\""
    for c in s.unicodeScalars {
        switch c {
        case "\"": out += "\\\""
        case "\\": out += "\\\\"
        case "\n": out += "\\n"
        case "\r": out += "\\r"
        case "\t": out += "\\t"
        default:
            if c.value < 0x20 {
                out += String(format: "\\u%04x", c.value)
            } else {
                out.unicodeScalars.append(c)
            }
        }
    }
    return out + "\""
}

struct TrackReport {
    var ok = false
    var mode = ""
    var device = ""
    var sampleRate = 0
    var channels = 0
    var bitsPerSample = 0
    var formatTag = 0
    var error = ""
    var status = ""
    /// Blocked on a person, not broken. See docs/protocol.md.
    var waiting = ""

    var json: String {
        var parts = ["\"ok\": \(ok)", "\"mode\": \(jsonString(mode))"]
        if !device.isEmpty { parts.append("\"device\": \(jsonString(device))") }
        if sampleRate > 0 { parts.append("\"sampleRate\": \(sampleRate)") }
        if channels > 0 { parts.append("\"channels\": \(channels)") }
        if bitsPerSample > 0 { parts.append("\"bitsPerSample\": \(bitsPerSample)") }
        if formatTag > 0 { parts.append("\"formatTag\": \(formatTag)") }
        if !error.isEmpty { parts.append("\"error\": \(jsonString(error))") }
        if !status.isEmpty { parts.append("\"hresult\": \(jsonString(status))") }
        if !waiting.isEmpty { parts.append("\"waiting\": \(jsonString(waiting))") }
        return "{" + parts.joined(separator: ", ") + "}"
    }
}

func report(_ format: AudioStreamBasicDescription, device: String, mode: String) -> TrackReport {
    var r = TrackReport()
    r.ok = true
    r.mode = mode
    r.device = device
    r.sampleRate = Int(format.mSampleRate)
    r.channels = Int(format.mChannelsPerFrame)
    r.bitsPerSample = Int(format.mBitsPerChannel)
    r.formatTag = format.mFormatFlags & kAudioFormatFlagIsFloat != 0 ? 3 : 1
    return r
}

// --- preflight -------------------------------------------------------------

/// Would a recording started now capture both sides of the meeting?
///
/// This opens *and starts* both endpoints rather than enumerating them. A
/// device that lists cleanly and then refuses to start is exactly the failure
/// worth catching before a meeting rather than after, and on this platform
/// there is a second one behind it: the system track's setup call can block
/// indefinitely on an audio-capture consent decision. That is reported as its
/// own outcome, because "waiting for permission" and "there is no such device"
/// need different things from whoever is about to lose a meeting.
func runPreflight(micOnly: Bool, systemOnly: Bool, appPID: pid_t?) -> Int32 {
    FrameWriter.shared.mute()   // stdout carries JSON in this mode, not frames
    var mic = TrackReport()
    var sys = TrackReport()

    if !systemOnly {
        let m = MicCapture()
        let openStatus = m.open()
        if openStatus != noErr {
            mic.mode = "input"
            mic.error = "the microphone could not be opened"
            mic.status = describe(openStatus)
        } else {
            let startStatus = m.start()
            if startStatus != noErr {
                mic.mode = "input"
                mic.device = m.deviceLabel
                mic.error = "the microphone opened but would not start"
                mic.status = describe(startStatus)
            } else {
                mic = report(m.streamFormat, device: m.deviceLabel, mode: "input")
            }
        }
        m.close()
    } else {
        mic.ok = true
        mic.mode = "skipped"
    }

    if !micOnly {
        let s = SystemCapture(appPID: appPID)
        let openStatus = s.open()
        if openStatus != noErr {
            sys.mode = appPID != nil ? "process tap" : "global tap"
            if s.timedOutWaiting {
                // A wait, not a fault: the helper is ready and macOS is holding
                // it until somebody answers. Deliberately no `error` — a track
                // carrying both would be read as broken, and the operator would
                // go looking for something to fix.
                sys.waiting = "system audio capture is waiting for permission — look for a dialog"
            } else {
                sys.error = "the system audio tap could not be created"
                sys.status = describe(openStatus)
            }
        } else {
            let startStatus = s.start()
            if startStatus != noErr {
                sys.mode = appPID != nil ? "process tap" : "global tap"
                sys.device = s.deviceLabel
                if s.timedOutWaiting {
                    sys.waiting = "system audio capture is waiting for permission — look for a dialog"
                } else {
                    sys.error = "the system audio tap was created but would not start"
                    sys.status = describe(startStatus)
                }
            } else {
                sys = report(s.streamFormat, device: s.deviceLabel,
                             mode: appPID != nil ? "process tap" : "global tap")
            }
        }
        s.close()
    } else {
        sys.ok = true
        sys.mode = "skipped"
    }

    let ok = mic.ok && sys.ok
    print("{\"platform\": \"macos\", \"tracks\": {\"microphone\": \(mic.json), " +
          "\"system\": \(sys.json)}, \"ok\": \(ok)}")
    return ok ? 0 : 1
}

// --- list-apps -------------------------------------------------------------

func runListApps() -> Int32 {
    FrameWriter.shared.mute()   // stdout carries JSON in this mode, not frames
    let apps = listAudioApps()
    let items = apps.map {
        "{\"pid\": \($0.pid), \"name\": \(jsonString($0.name)), \"active\": \($0.active)}"
    }
    print("{\"apps\": [\(items.joined(separator: ", "))]}")
    return 0
}

// --- recording -------------------------------------------------------------

func runCapture(micOnly: Bool, systemOnly: Bool, durationMs: Int, appPID: pid_t?) -> Int32 {
    var mic: MicCapture?
    var sys: SystemCapture?
    var failures: [String] = []

    if !systemOnly {
        let m = MicCapture()
        let st = m.open()
        if st != noErr {
            FrameWriter.shared.log(track: .mic, "microphone could not be opened: \(describe(st))")
            failures.append("microphone: \(describe(st))")
            m.close()
        } else {
            let started = m.start()
            if started != noErr {
                FrameWriter.shared.log(track: .mic, "microphone would not start: \(describe(started))")
                failures.append("microphone: \(describe(started))")
                m.close()
            } else {
                mic = m
            }
        }
    }

    if !micOnly {
        let s = SystemCapture(appPID: appPID)
        let st = s.open()
        if st != noErr {
            let why = st == kAudioDevicePermissionsError
                ? "system audio capture is waiting for permission and nothing answered it"
                : "system audio tap could not be created: \(describe(st))"
            FrameWriter.shared.log(track: .system, why)
            failures.append("system: \(describe(st))")
            s.close()
        } else {
            let started = s.start()
            if started != noErr {
                FrameWriter.shared.log(track: .system, "system audio tap would not start: \(describe(started))")
                failures.append("system: \(describe(started))")
                s.close()
            } else {
                sys = s
            }
        }
    }

    // Refusing here rather than recording is the point. Half a meeting looks
    // entirely successful until somebody tries to read the notes.
    if !failures.isEmpty {
        mic?.close()
        sys?.close()
        FileHandle.standardError.write(
            Data(("minutes-capture: refusing to record — " + failures.joined(separator: "; ") + "\n").utf8))
        return 1
    }

    // Stopping is stdin closing. A duration, when given, is a second way to
    // reach the same clean stop.
    let deadline = durationMs > 0 ? Date().addingTimeInterval(Double(durationMs) / 1000.0) : nil
    var buf = [UInt8](repeating: 0, count: 256)
    var running = true
    while running {
        if let deadline, Date() >= deadline { break }
        if let m = mic, m.state.failure != nil { break }
        if let s = sys, s.state.failure != nil { break }

        var fds = pollfd(fd: 0, events: Int16(POLLIN), revents: 0)
        let timeoutMs: Int32 = 200
        let n = poll(&fds, 1, timeoutMs)
        if n > 0 {
            let got = read(0, &buf, buf.count)
            if got <= 0 { running = false }   // EOF: the orchestrator said stop
        } else if n < 0 && errno != EINTR {
            running = false
        }
    }

    // Finish the packet in hand, then say where each track got to.
    mic?.stop()
    sys?.stop()
    let now = nowIn100ns()
    if let m = mic { FrameWriter.shared.end(track: .mic, qpc100ns: now, framesDelivered: m.state.framesDelivered) }
    if let s = sys { FrameWriter.shared.end(track: .system, qpc100ns: now, framesDelivered: s.state.framesDelivered) }

    let micFailure = mic?.state.failure
    let sysFailure = sys?.state.failure
    mic?.close()
    sys?.close()

    // A capture that died is not a capture that ended.
    if let micFailure {
        FileHandle.standardError.write(Data(("minutes-capture: " + micFailure + "\n").utf8))
    }
    if let sysFailure {
        FileHandle.standardError.write(Data(("minutes-capture: " + sysFailure + "\n").utf8))
    }
    return (micFailure == nil && sysFailure == nil) ? 0 : 1
}

// --- arguments -------------------------------------------------------------

func usage() {
    print("""
    minutes-capture — capture both sides of a meeting as two framed tracks

      --preflight        report whether both tracks can be captured, as JSON
      --list-apps        list processes producing audio, as JSON
      --duration-ms N    stop after N milliseconds (default: run until stdin closes)
      --mic-only         capture track 0 only
      --system-only      capture track 1 only
      --app-pid N        capture only process N, rather than everything played

    Audio is written to stdout as framed chunks; see docs/protocol.md.
    """)
}

var doPreflight = false
var doListApps = false
var micOnly = false
var systemOnly = false
var durationMs = 0
var appPID: pid_t?

var args = Array(CommandLine.arguments.dropFirst())
var i = 0
while i < args.count {
    let a = args[i]
    switch a {
    case "--preflight":  doPreflight = true
    case "--list-apps":  doListApps = true
    case "--mic-only":   micOnly = true
    case "--system-only": systemOnly = true
    case "--duration-ms":
        if i + 1 < args.count { i += 1; durationMs = Int(args[i]) ?? 0 }
    case "--app-pid":
        if i + 1 < args.count { i += 1; appPID = pid_t(args[i]) ?? nil }
    case "--help", "-h":
        usage()
        exit(0)
    default:
        FileHandle.standardError.write(Data("minutes-capture: unknown argument \(a)\n".utf8))
        usage()
        exit(2)
    }
    i += 1
}

if micOnly && systemOnly {
    FileHandle.standardError.write(Data("minutes-capture: --mic-only and --system-only are contradictory\n".utf8))
    exit(2)
}

if doPreflight {
    exit(runPreflight(micOnly: micOnly, systemOnly: systemOnly, appPID: appPID))
}
if doListApps {
    exit(runListApps())
}
exit(runCapture(micOnly: micOnly, systemOnly: systemOnly, durationMs: durationMs, appPID: appPID))
