// The framed stdout protocol, writing half.
//
// docs/protocol.md is the specification and internal/frame/frame.go is the
// reader this must satisfy. Neither is changed for this platform: the reader is
// shared with the Windows helper, so anything written here that the Go side
// cannot parse is a bug here.

import Foundation
import CoreAudio

enum FrameType: UInt16 {
    case trackInfo = 1
    case audio     = 2
    case end       = 3
    case log       = 4
}

enum TrackID: UInt16 {
    case mic    = 0
    case system = 1

    var name: String { self == .mic ? "mic" : "system" }
}

// Frame flags. On an AUDIO frame these describe the packet; on a TRACK_INFO
// frame bit 0 means something else entirely — see FrameWriter.trackInfo.
struct FrameFlags {
    static let silent: UInt32         = 1 << 0
    static let discontinuity: UInt32  = 1 << 1
    static let timestampError: UInt32 = 1 << 2
    // On TRACK_INFO only: this track captures one process rather than the whole
    // machine, so its device counter measures frames delivered rather than time
    // elapsed and must not be used to place audio.
    static let processScoped: UInt32  = 1 << 0
}

private let frameMagic: UInt32 = 0x314E494D // "MIN1"
private let headerSize = 32

/// Serialises every frame onto stdout.
///
/// Both tracks write from their own real-time threads, so the lock is not
/// optional: two interleaved writes would desynchronise the stream, and the
/// reader would fail on a magic that is really the middle of somebody's audio.
final class FrameWriter: @unchecked Sendable {
    private let lock = NSLock()
    private var muted = false

    static let shared = FrameWriter()

    /// Stops anything reaching stdout.
    ///
    /// `--preflight` and `--list-apps` print JSON on stdout, and a diagnostic
    /// frame written during either turns the report into bytes beginning
    /// "MIN1" — which the orchestrator reports as "could not read the helper's
    /// report: invalid character 'M'". One stdout cannot carry two formats, so
    /// the JSON modes take it exclusively.
    func mute() {
        lock.lock(); muted = true; lock.unlock()
    }

    private func write(type: FrameType, track: TrackID, qpc100ns: UInt64,
                       devicePos: UInt64, flags: UInt32, payload: UnsafeRawBufferPointer?) {
        var header = Data(count: headerSize)
        let payloadLen = UInt32(payload?.count ?? 0)
        header.withUnsafeMutableBytes { raw in
            let p = raw.baseAddress!
            p.storeBytes(of: frameMagic.littleEndian, toByteOffset: 0, as: UInt32.self)
            p.storeBytes(of: type.rawValue.littleEndian, toByteOffset: 4, as: UInt16.self)
            p.storeBytes(of: track.rawValue.littleEndian, toByteOffset: 6, as: UInt16.self)
            p.storeBytes(of: qpc100ns.littleEndian, toByteOffset: 8, as: UInt64.self)
            p.storeBytes(of: devicePos.littleEndian, toByteOffset: 16, as: UInt64.self)
            p.storeBytes(of: payloadLen.littleEndian, toByteOffset: 24, as: UInt32.self)
            p.storeBytes(of: flags.littleEndian, toByteOffset: 28, as: UInt32.self)
        }

        lock.lock()
        defer { lock.unlock() }
        if muted { return }
        var buf = header
        if let payload, payload.count > 0 {
            buf.append(contentsOf: payload)
        }
        buf.withUnsafeBytes { raw in
            var off = 0
            while off < raw.count {
                let n = Darwin.write(1, raw.baseAddress!.advanced(by: off), raw.count - off)
                if n <= 0 {
                    // The orchestrator has gone. Nothing useful to report to,
                    // and nothing to gain by looping.
                    if errno == EINTR { continue }
                    return
                }
                off += n
            }
        }
    }

    /// TRACK_INFO, emitted once per track before any of its audio.
    func trackInfo(track: TrackID, format: AudioStreamBasicDescription,
                   device: String, processScoped: Bool, qpc100ns: UInt64) {
        let nameBytes = Array(device.utf8)
        var payload = Data(count: 24)
        let isFloat = format.mFormatFlags & kAudioFormatFlagIsFloat != 0
        payload.withUnsafeMutableBytes { raw in
            let p = raw.baseAddress!
            p.storeBytes(of: UInt32(format.mSampleRate).littleEndian, toByteOffset: 0, as: UInt32.self)
            p.storeBytes(of: UInt16(format.mChannelsPerFrame).littleEndian, toByteOffset: 4, as: UInt16.self)
            p.storeBytes(of: UInt16(format.mBitsPerChannel).littleEndian, toByteOffset: 6, as: UInt16.self)
            p.storeBytes(of: UInt16(isFloat ? 3 : 1).littleEndian, toByteOffset: 8, as: UInt16.self)
            p.storeBytes(of: UInt16(format.mBytesPerFrame).littleEndian, toByteOffset: 10, as: UInt16.self)
            // qpcFrequency is informational and named for the Windows clock.
            // Host time is converted to 100ns units before it is sent, so the
            // honest value here is that unit's frequency.
            p.storeBytes(of: UInt64(10_000_000).littleEndian, toByteOffset: 12, as: UInt64.self)
            p.storeBytes(of: UInt32(nameBytes.count).littleEndian, toByteOffset: 20, as: UInt32.self)
        }
        payload.append(contentsOf: nameBytes)
        payload.withUnsafeBytes { raw in
            write(type: .trackInfo, track: track, qpc100ns: qpc100ns, devicePos: 0,
                  flags: processScoped ? FrameFlags.processScoped : 0, payload: raw)
        }
    }

    func audio(track: TrackID, qpc100ns: UInt64, devicePos: UInt64, flags: UInt32,
               bytes: UnsafeRawBufferPointer) {
        write(type: .audio, track: track, qpc100ns: qpc100ns, devicePos: devicePos,
              flags: flags, payload: bytes)
    }

    func end(track: TrackID, qpc100ns: UInt64, framesDelivered: UInt64) {
        write(type: .end, track: track, qpc100ns: qpc100ns, devicePos: framesDelivered,
              flags: 0, payload: nil)
    }

    /// Diagnostics travel in-band so a failure keeps its position in the
    /// timeline. The orchestrator carries the last one per track into the
    /// manifest, which is how a failed recording says what happened to it.
    func log(track: TrackID, _ message: String) {
        let bytes = Array(message.utf8)
        bytes.withUnsafeBytes { raw in
            write(type: .log, track: track, qpc100ns: nowIn100ns(), devicePos: 0,
                  flags: 0, payload: raw)
        }
    }
}

// --- the shared clock ------------------------------------------------------

/// Host time in 100-nanosecond units.
///
/// This is the only thing the two tracks have in common, so it is what relates
/// one to the other. Both CoreAudio callbacks report `mHostTime` from this same
/// clock, exactly as WASAPI reports a QueryPerformanceCounter position, so
/// alignment stays arithmetic rather than a waveform comparison.
@inline(__always)
func hostTimeTo100ns(_ hostTime: UInt64) -> UInt64 {
    AudioConvertHostTimeToNanos(hostTime) / 100
}

@inline(__always)
func nowIn100ns() -> UInt64 {
    hostTimeTo100ns(AudioGetCurrentHostTime())
}

// --- naming failures -------------------------------------------------------

/// An OSStatus rendered as something a person can act on.
///
/// A recording that failed must say what happened to it. Eight digits of
/// decimal, or a four-character code printed as an integer, is not that — and
/// on Windows this exact gap turned "the audio device was removed" into
/// "exit status 1".
func describe(_ status: OSStatus) -> String {
    switch status {
    case noErr: return "noErr"
    case kAudioHardwareNotRunningError:        return "kAudioHardwareNotRunningError (the audio server is not running)"
    case kAudioHardwareUnspecifiedError:       return "kAudioHardwareUnspecifiedError"
    case kAudioHardwareUnknownPropertyError:   return "kAudioHardwareUnknownPropertyError"
    case kAudioHardwareBadPropertySizeError:   return "kAudioHardwareBadPropertySizeError"
    case kAudioHardwareIllegalOperationError:  return "kAudioHardwareIllegalOperationError"
    case kAudioHardwareBadObjectError:         return "kAudioHardwareBadObjectError (the object no longer exists)"
    case kAudioHardwareBadDeviceError:         return "kAudioHardwareBadDeviceError (the device was removed)"
    case kAudioHardwareBadStreamError:         return "kAudioHardwareBadStreamError"
    case kAudioHardwareUnsupportedOperationError: return "kAudioHardwareUnsupportedOperationError"
    case kAudioHardwareNotReadyError:          return "kAudioHardwareNotReadyError"
    case kAudioDeviceUnsupportedFormatError:   return "kAudioDeviceUnsupportedFormatError"
    case kAudioDevicePermissionsError:         return "kAudioDevicePermissionsError (consent was refused)"
    case OSStatus(-50):                        return "paramErr (-50)"
    default:
        let u = UInt32(bitPattern: status)
        let b = [UInt8((u >> 24) & 0xff), UInt8((u >> 16) & 0xff),
                 UInt8((u >> 8) & 0xff), UInt8(u & 0xff)]
        if b.allSatisfy({ $0 >= 0x20 && $0 < 0x7f }),
           let s = String(bytes: b, encoding: .ascii) {
            return "OSStatus \(status) ('\(s)')"
        }
        return "OSStatus \(status)"
    }
}
