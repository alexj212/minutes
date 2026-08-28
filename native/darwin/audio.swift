// Capturing the two tracks.
//
// Track 0 is the microphone, through a HAL audio unit on the default input
// device. Track 1 is everything the machine plays, through a CoreAudio process
// tap read via a private aggregate device.
//
// Both are captured in the device's own format and converted here not at all:
// the orchestrator converts for storage, and a helper that resamples loses
// information it cannot return.

import Foundation
import CoreAudio
import AudioToolbox

// --- property helpers ------------------------------------------------------

func audioObjectProperty<T>(_ obj: AudioObjectID, _ selector: AudioObjectPropertySelector,
                            _ scope: AudioObjectPropertyScope = kAudioObjectPropertyScopeGlobal,
                            _ initial: T) -> (T, OSStatus) {
    var addr = AudioObjectPropertyAddress(mSelector: selector, mScope: scope,
                                          mElement: kAudioObjectPropertyElementMain)
    var size = UInt32(MemoryLayout<T>.size)
    var value = initial
    let st = withUnsafeMutablePointer(to: &value) {
        AudioObjectGetPropertyData(obj, &addr, 0, nil, &size, $0)
    }
    return (value, st)
}

func audioObjectString(_ obj: AudioObjectID, _ selector: AudioObjectPropertySelector) -> String? {
    var addr = AudioObjectPropertyAddress(mSelector: selector,
                                          mScope: kAudioObjectPropertyScopeGlobal,
                                          mElement: kAudioObjectPropertyElementMain)
    var size = UInt32(MemoryLayout<CFString?>.size)
    var value: CFString? = nil
    let st = withUnsafeMutablePointer(to: &value) {
        AudioObjectGetPropertyData(obj, &addr, 0, nil, &size, $0)
    }
    guard st == noErr, let v = value else { return nil }
    return v as String
}

func audioObjectArray<T: FixedWidthInteger>(_ obj: AudioObjectID,
                                            _ selector: AudioObjectPropertySelector,
                                            _ zero: T) -> [T] {
    var addr = AudioObjectPropertyAddress(mSelector: selector,
                                          mScope: kAudioObjectPropertyScopeGlobal,
                                          mElement: kAudioObjectPropertyElementMain)
    var size: UInt32 = 0
    guard AudioObjectGetPropertyDataSize(obj, &addr, 0, nil, &size) == noErr, size > 0 else { return [] }
    var out = [T](repeating: zero, count: Int(size) / MemoryLayout<T>.size)
    let st = out.withUnsafeMutableBytes {
        AudioObjectGetPropertyData(obj, &addr, 0, nil, &size, $0.baseAddress!)
    }
    guard st == noErr else { return [] }
    return out
}

func defaultDevice(input: Bool) -> AudioObjectID {
    let sel = input ? kAudioHardwarePropertyDefaultInputDevice
                    : kAudioHardwarePropertyDefaultOutputDevice
    let (dev, st) = audioObjectProperty(AudioObjectID(kAudioObjectSystemObject), sel,
                                        kAudioObjectPropertyScopeGlobal,
                                        AudioObjectID(kAudioObjectUnknown))
    return st == noErr ? dev : AudioObjectID(kAudioObjectUnknown)
}

func deviceName(_ dev: AudioObjectID) -> String {
    audioObjectString(dev, kAudioObjectPropertyName) ?? "(unnamed device)"
}

func deviceUID(_ dev: AudioObjectID) -> String? {
    audioObjectString(dev, kAudioDevicePropertyDeviceUID)
}

// --- bounding a call that can hang -----------------------------------------

enum BoundedResult<T> {
    case value(T)
    case timedOut
}

/// Runs `work` on its own thread and gives up waiting after `seconds`.
///
/// This exists because of a measured failure rather than caution.
/// `AudioDeviceCreateIOProcIDWithBlock` on a tap-bearing aggregate does not
/// return slowly, it does not return at all: the stack sits in
/// `HALC_ProxyIOContext::_TellServerAboutStreamUsage` inside a synchronous
/// mach_msg to coreaudiod. The identical call on an aggregate with no tap
/// returns instantly, so this is the tap path specifically, and the most likely
/// cause is an audio-capture consent decision nobody is present to make.
///
/// A helper that blocks there produces no report at all, and the orchestrator
/// then tells the operator "the capture helper produced no report", which is
/// true and useless. Timing out and saying *why* is the whole point.
///
/// The abandoned thread is not cleaned up. It cannot be — there is no way to
/// interrupt a synchronous mach message — so the process must exit rather than
/// carry on and hope.
private final class ResultBox<U>: @unchecked Sendable {
    var value: U?
}

func bounded<T>(seconds: Double, _ work: @escaping () -> T) -> BoundedResult<T> {
    let sem = DispatchSemaphore(value: 0)
    let box = ResultBox<T>()
    let t = Thread {
        box.value = work()
        sem.signal()
    }
    t.stackSize = 512 * 1024
    t.start()
    if sem.wait(timeout: .now() + seconds) == .timedOut {
        return .timedOut
    }
    return .value(box.value!)
}

// --- how a track reports itself --------------------------------------------

final class TrackState: @unchecked Sendable {
    let id: TrackID
    private let lock = NSLock()
    private var _failure: String?
    private var _framesDelivered: UInt64 = 0
    private var _sentTrackInfo = false

    init(_ id: TrackID) { self.id = id }

    var failure: String? {
        lock.lock(); defer { lock.unlock() }
        return _failure
    }

    var framesDelivered: UInt64 {
        lock.lock(); defer { lock.unlock() }
        return _framesDelivered
    }

    func addFrames(_ n: UInt64) {
        lock.lock(); _framesDelivered += n; lock.unlock()
    }

    /// A mid-stream error is not the end of a recording, it is a recording cut
    /// in half. Marking the track failed is what makes the process exit
    /// non-zero, which is what stops the orchestrator recording a truncated
    /// meeting as a complete one.
    func fail(_ reason: String) {
        lock.lock()
        let first = _failure == nil
        if first { _failure = reason }
        lock.unlock()
        if first { FrameWriter.shared.log(track: id, reason) }
    }

    func needsTrackInfo() -> Bool {
        lock.lock(); defer { lock.unlock() }
        if _sentTrackInfo { return false }
        _sentTrackInfo = true
        return true
    }
}

// --- track 0: the microphone -----------------------------------------------

final class MicCapture: @unchecked Sendable {
    let state = TrackState(.mic)
    private var unit: AudioUnit?
    private var format = AudioStreamBasicDescription()
    private var device = AudioObjectID(kAudioObjectUnknown)
    private var buffers: UnsafeMutableAudioBufferListPointer?
    private var scratch: UnsafeMutableRawPointer?
    private var scratchBytes = 0
    private var started = false

    var deviceLabel: String { deviceName(device) }
    var streamFormat: AudioStreamBasicDescription { format }

    /// Opens the default input device and prepares the unit. Does not start it.
    func open() -> OSStatus {
        device = defaultDevice(input: true)
        if device == kAudioObjectUnknown {
            return kAudioHardwareBadDeviceError
        }

        var desc = AudioComponentDescription(
            componentType: kAudioUnitType_Output,
            componentSubType: kAudioUnitSubType_HALOutput,
            componentManufacturer: kAudioUnitManufacturer_Apple,
            componentFlags: 0, componentFlagsMask: 0)
        guard let comp = AudioComponentFindNext(nil, &desc) else {
            return kAudioHardwareUnspecifiedError
        }
        var u: AudioUnit?
        var st = AudioComponentInstanceNew(comp, &u)
        guard st == noErr, let unit = u else { return st }
        self.unit = unit

        var enable: UInt32 = 1
        st = AudioUnitSetProperty(unit, kAudioOutputUnitProperty_EnableIO,
                                  kAudioUnitScope_Input, 1, &enable, UInt32(MemoryLayout<UInt32>.size))
        if st != noErr { return st }
        var disable: UInt32 = 0
        st = AudioUnitSetProperty(unit, kAudioOutputUnitProperty_EnableIO,
                                  kAudioUnitScope_Output, 0, &disable, UInt32(MemoryLayout<UInt32>.size))
        if st != noErr { return st }

        var dev = device
        st = AudioUnitSetProperty(unit, kAudioOutputUnitProperty_CurrentDevice,
                                  kAudioUnitScope_Global, 0, &dev, UInt32(MemoryLayout<AudioObjectID>.size))
        if st != noErr { return st }

        // Take the device's own format rather than imposing one. Converting
        // here would throw away information the orchestrator might want and
        // cannot recover.
        var size = UInt32(MemoryLayout<AudioStreamBasicDescription>.size)
        st = AudioUnitGetProperty(unit, kAudioUnitProperty_StreamFormat,
                                  kAudioUnitScope_Input, 1, &format, &size)
        if st != noErr { return st }
        st = AudioUnitSetProperty(unit, kAudioUnitProperty_StreamFormat,
                                  kAudioUnitScope_Output, 1, &format, size)
        if st != noErr { return st }

        var callback = AURenderCallbackStruct(
            inputProc: { inRefCon, ioActionFlags, inTimeStamp, inBusNumber, inNumberFrames, _ in
                let me = Unmanaged<MicCapture>.fromOpaque(inRefCon).takeUnretainedValue()
                return me.render(ioActionFlags, inTimeStamp, inBusNumber, inNumberFrames)
            },
            inputProcRefCon: Unmanaged.passUnretained(self).toOpaque())
        st = AudioUnitSetProperty(unit, kAudioOutputUnitProperty_SetInputCallback,
                                  kAudioUnitScope_Global, 0, &callback,
                                  UInt32(MemoryLayout<AURenderCallbackStruct>.size))
        if st != noErr { return st }

        return AudioUnitInitialize(unit)
    }

    func start() -> OSStatus {
        guard let unit else { return kAudioHardwareBadObjectError }
        let st = AudioOutputUnitStart(unit)
        if st == noErr {
            started = true
            emitTrackInfo()
        }
        return st
    }

    /// TRACK_INFO once the endpoint is running, not when audio first arrives.
    ///
    /// Matching capture.cpp, which emits it before `IAudioClient::Start` so a
    /// reader never sees samples it cannot interpret. Waiting for the first
    /// packet instead loses the track entirely when the endpoint stays quiet,
    /// and the orchestrator then builds a manifest with no such track rather
    /// than a track of silence. Those are different things downstream: one is
    /// a meeting where nobody spoke, the other is a recorder that was not
    /// listening.
    func emitTrackInfo() {
        guard state.needsTrackInfo() else { return }
        FrameWriter.shared.trackInfo(track: .mic, format: format, device: deviceLabel,
                                     processScoped: false, qpc100ns: 0)
    }

    func stop() {
        guard let unit else { return }
        if started { AudioOutputUnitStop(unit); started = false }
    }

    func close() {
        stop()
        if let unit { AudioUnitUninitialize(unit); AudioComponentInstanceDispose(unit) }
        unit = nil
        if let scratch { scratch.deallocate() }
        scratch = nil
        if let buffers { free(buffers.unsafeMutablePointer) }
        buffers = nil
    }

    private func ensureScratch(frames: UInt32) {
        let needed = Int(frames) * Int(format.mBytesPerFrame)
        if scratchBytes >= needed, buffers != nil { return }
        if let scratch { scratch.deallocate() }
        if let buffers { free(buffers.unsafeMutablePointer) }
        scratchBytes = needed
        scratch = UnsafeMutableRawPointer.allocate(byteCount: needed, alignment: 16)
        let abl = AudioBufferList.allocate(maximumBuffers: 1)
        abl[0] = AudioBuffer(mNumberChannels: format.mChannelsPerFrame,
                             mDataByteSize: UInt32(needed), mData: scratch)
        buffers = abl
    }

    private func render(_ flags: UnsafeMutablePointer<AudioUnitRenderActionFlags>,
                        _ ts: UnsafePointer<AudioTimeStamp>,
                        _ bus: UInt32, _ frames: UInt32) -> OSStatus {
        guard let unit else { return noErr }
        if state.failure != nil { return noErr }

        ensureScratch(frames: frames)
        guard let abl = buffers else { return noErr }
        abl[0].mDataByteSize = frames * format.mBytesPerFrame

        let st = AudioUnitRender(unit, flags, ts, bus, frames, abl.unsafeMutablePointer)
        if st != noErr {
            // Failing to open is one thing; dying mid-stream is another, and
            // the difference is a meeting cut in half reported as a meeting
            // that ended there.
            state.fail("microphone capture stopped mid-stream: \(describe(st))")
            return st
        }

        let stamp = ts.pointee
        let host = stamp.mFlags.contains(.hostTimeValid) ? stamp.mHostTime : AudioGetCurrentHostTime()
        let samplePos = stamp.mFlags.contains(.sampleTimeValid) ? UInt64(max(0, stamp.mSampleTime)) : state.framesDelivered
        var frameFlags: UInt32 = 0
        if !stamp.mFlags.contains(.hostTimeValid) { frameFlags |= FrameFlags.timestampError }

        let byteCount = Int(abl[0].mDataByteSize)
        if let data = abl[0].mData, byteCount > 0 {
            FrameWriter.shared.audio(track: .mic, qpc100ns: hostTimeTo100ns(host),
                                     devicePos: samplePos, flags: frameFlags,
                                     bytes: UnsafeRawBufferPointer(start: data, count: byteCount))
            state.addFrames(UInt64(frames))
        }
        return noErr
    }
}

// --- track 1: everything the machine plays ---------------------------------

/// A process tap read through a private aggregate device.
///
/// Two properties matter and both are deliberate. The tap is created
/// `CATapUnmuted`, so audio is captured *and* still reaches the hardware — the
/// refused failure here is the recording succeeding while the human stops
/// hearing the meeting, and `CATapMuted` is exactly that failure available as
/// a one-line mistake. The aggregate is private, so it does not appear in the
/// user's sound settings and cannot become anybody's default device.
final class SystemCapture: @unchecked Sendable {
    let state = TrackState(.system)
    private var tapID = AudioObjectID(kAudioObjectUnknown)
    private var aggID = AudioObjectID(kAudioObjectUnknown)
    private var procID: AudioDeviceIOProcID?
    private var format = AudioStreamBasicDescription()
    private var started = false
    private let processScoped: Bool
    private var label = "system audio"
    /// Set when a setup call was abandoned on its timeout rather than failing.
    ///
    /// Kept separate from any OSStatus, because "nobody has answered the dialog
    /// yet" and "permission was refused" are different answers and the operator
    /// does different things about them. Reusing one status for both is the
    /// collapse the waiting state exists to prevent.
    private(set) var timedOutWaiting = false

    init(appPID: pid_t?) {
        self.processScoped = appPID != nil
        self.appPID = appPID
    }
    private let appPID: pid_t?

    var deviceLabel: String { label }
    var streamFormat: AudioStreamBasicDescription { format }
    var isProcessScoped: Bool { processScoped }

    func open() -> OSStatus {
        let desc: CATapDescription
        if let pid = appPID {
            let (obj, st) = processObject(forPID: pid)
            if st != noErr { return st }
            desc = CATapDescription(stereoMixdownOfProcesses: [obj])
            label = "system audio (pid \(pid))"
        } else {
            // Excluding nothing is the whole machine — the direct equivalent of
            // system-wide WASAPI loopback.
            desc = CATapDescription(stereoGlobalTapButExcludeProcesses: [])
            label = "system audio (all processes)"
        }
        desc.name = "minutes capture"
        desc.uuid = UUID()
        desc.isPrivate = true
        desc.muteBehavior = .unmuted

        var tap = AudioObjectID(kAudioObjectUnknown)
        var st = AudioHardwareCreateProcessTap(desc, &tap)
        if st != noErr { return st }
        tapID = tap

        var fmtAddr = AudioObjectPropertyAddress(mSelector: kAudioTapPropertyFormat,
                                                 mScope: kAudioObjectPropertyScopeGlobal,
                                                 mElement: kAudioObjectPropertyElementMain)
        var size = UInt32(MemoryLayout<AudioStreamBasicDescription>.size)
        st = AudioObjectGetPropertyData(tapID, &fmtAddr, 0, nil, &size, &format)
        if st != noErr { return st }

        // The aggregate carries the default output device as a sub-device
        // purely as a clock source; it is not routed through and the user's
        // default output is never changed. A tap-only aggregate creates and
        // starts without error and then delivers nothing, which is the
        // enumerate-versus-start failure this project exists to refuse.
        let outUID = deviceUID(defaultDevice(input: false)) ?? ""
        var aggDesc: [String: Any] = [
            kAudioAggregateDeviceNameKey: "minutes capture",
            kAudioAggregateDeviceUIDKey: "minutes-\(UUID().uuidString)",
            kAudioAggregateDeviceIsPrivateKey: true,
            kAudioAggregateDeviceIsStackedKey: false,
            kAudioAggregateDeviceTapAutoStartKey: true,
            kAudioAggregateDeviceTapListKey: [[
                kAudioSubTapDriftCompensationKey: true,
                kAudioSubTapUIDKey: desc.uuid.uuidString,
            ]],
        ]
        if !outUID.isEmpty {
            aggDesc[kAudioAggregateDeviceMainSubDeviceKey] = outUID
            aggDesc[kAudioAggregateDeviceSubDeviceListKey] = [[kAudioSubDeviceUIDKey: outUID]]
        } else {
            aggDesc[kAudioAggregateDeviceSubDeviceListKey] = []
        }

        var agg = AudioObjectID(kAudioObjectUnknown)
        st = AudioHardwareCreateAggregateDevice(aggDesc as CFDictionary, &agg)
        if st != noErr { return st }
        aggID = agg

        var inFmt = AudioStreamBasicDescription()
        var inSize = UInt32(MemoryLayout<AudioStreamBasicDescription>.size)
        var inAddr = AudioObjectPropertyAddress(mSelector: kAudioDevicePropertyStreamFormat,
                                                mScope: kAudioObjectPropertyScopeInput,
                                                mElement: kAudioObjectPropertyElementMain)
        if AudioObjectGetPropertyData(aggID, &inAddr, 0, nil, &inSize, &inFmt) == noErr,
           inFmt.mSampleRate > 0 {
            format = inFmt
        }

        // The rate both obvious properties report is not the rate audio
        // arrives at, and this was measured rather than reasoned about.
        // `kAudioTapPropertyFormat` and the aggregate's input `StreamFormat`
        // both said 48000 while the tap delivered 44100 — the rate of the
        // output device the aggregate is clocked by. Only
        // `kAudioDevicePropertyNominalSampleRate` on the aggregate told the
        // truth.
        //
        // Believing the wrong one is not a subtle bug. TRACK_INFO would
        // declare 48000, the orchestrator would write a WAV header saying
        // 48000 over samples that are 44100, and the whole recording plays
        // 8% fast with every transcript timestamp sliding — about ten minutes
        // of drift across a two-hour call, in a file that opens and plays and
        // looks entirely fine.
        let (nominal, nominalStatus) = audioObjectProperty(aggID,
            kAudioDevicePropertyNominalSampleRate, kAudioObjectPropertyScopeGlobal, Float64(0))
        if nominalStatus == noErr, nominal > 0, nominal != format.mSampleRate {
            FrameWriter.shared.log(track: .system,
                "tap format declared \(Int(format.mSampleRate)) Hz but the device runs at " +
                "\(Int(nominal)) Hz; using the device rate")
            format.mSampleRate = nominal
        }

        // Bounded, because this call can block forever waiting on a consent
        // decision. See `bounded`.
        let outcome = bounded(seconds: 8.0) { () -> (OSStatus, AudioDeviceIOProcID?) in
            var proc: AudioDeviceIOProcID?
            let s = AudioDeviceCreateIOProcIDWithBlock(&proc, agg, nil) {
                [weak self] _, inInputData, inInputTime, _, _ in
                self?.deliver(inInputData, inInputTime)
            }
            return (s, proc)
        }
        switch outcome {
        case .timedOut:
            timedOutWaiting = true
            return kAudioDevicePermissionsError
        case .value(let (s, proc)):
            if s != noErr { return s }
            procID = proc
            return noErr
        }
    }

    func start() -> OSStatus {
        guard aggID != kAudioObjectUnknown, let proc = procID else {
            return kAudioHardwareBadObjectError
        }
        let agg = aggID
        let outcome = bounded(seconds: 8.0) { AudioDeviceStart(agg, proc) }
        switch outcome {
        case .timedOut:
            timedOutWaiting = true
            return kAudioDevicePermissionsError
        case .value(let s):
            if s == noErr {
                started = true
                emitTrackInfo()
            }
            return s
        }
    }

    /// See MicCapture.emitTrackInfo. This track needs it more: the render
    /// endpoint is idle until something plays, so a tap delivers no packets at
    /// all at the start of every recording — measured here, a ten second
    /// capture with nothing playing produced no system track whatsoever.
    func emitTrackInfo() {
        guard state.needsTrackInfo() else { return }
        FrameWriter.shared.trackInfo(track: .system, format: format, device: label,
                                     processScoped: processScoped, qpc100ns: 0)
    }

    func stop() {
        if started, let proc = procID { AudioDeviceStop(aggID, proc); started = false }
    }

    func close() {
        stop()
        if let proc = procID, aggID != kAudioObjectUnknown {
            AudioDeviceDestroyIOProcID(aggID, proc)
        }
        procID = nil
        if aggID != kAudioObjectUnknown { AudioHardwareDestroyAggregateDevice(aggID) }
        aggID = kAudioObjectUnknown
        if tapID != kAudioObjectUnknown { AudioHardwareDestroyProcessTap(tapID) }
        tapID = kAudioObjectUnknown
    }

    private func deliver(_ data: UnsafePointer<AudioBufferList>,
                         _ time: UnsafePointer<AudioTimeStamp>) {
        if state.failure != nil { return }
        let stamp = time.pointee
        let host = stamp.mFlags.contains(.hostTimeValid) ? stamp.mHostTime : AudioGetCurrentHostTime()
        var frameFlags: UInt32 = 0
        if !stamp.mFlags.contains(.hostTimeValid) { frameFlags |= FrameFlags.timestampError }

        // A process-scoped tap delivers nothing while its target is quiet, so
        // its sample counter measures frames delivered rather than time
        // elapsed. Sending it as a position would make the orchestrator
        // re-anchor continuously; the flag on TRACK_INFO tells it to place this
        // track by wall-clock alone.
        let samplePos = (!processScoped && stamp.mFlags.contains(.sampleTimeValid))
            ? UInt64(max(0, stamp.mSampleTime))
            : state.framesDelivered

        let abl = UnsafeMutableAudioBufferListPointer(UnsafeMutablePointer(mutating: data))
        for buf in abl {
            guard let d = buf.mData, buf.mDataByteSize > 0 else { continue }
            FrameWriter.shared.audio(track: .system, qpc100ns: hostTimeTo100ns(host),
                                     devicePos: samplePos, flags: frameFlags,
                                     bytes: UnsafeRawBufferPointer(start: d, count: Int(buf.mDataByteSize)))
            if format.mBytesPerFrame > 0 {
                state.addFrames(UInt64(buf.mDataByteSize / format.mBytesPerFrame))
            }
        }
    }
}

// --- processes the audio engine knows about --------------------------------

func processObject(forPID pid: pid_t) -> (AudioObjectID, OSStatus) {
    var addr = AudioObjectPropertyAddress(mSelector: kAudioHardwarePropertyTranslatePIDToProcessObject,
                                          mScope: kAudioObjectPropertyScopeGlobal,
                                          mElement: kAudioObjectPropertyElementMain)
    var inPID = pid
    var obj = AudioObjectID(kAudioObjectUnknown)
    var size = UInt32(MemoryLayout<AudioObjectID>.size)
    let st = AudioObjectGetPropertyData(AudioObjectID(kAudioObjectSystemObject), &addr,
                                        UInt32(MemoryLayout<pid_t>.size), &inPID, &size, &obj)
    return (obj, st)
}

struct AudioApp {
    let pid: Int
    let name: String
    let active: Bool
}

/// Everything with an audio session, and whether it is actually making noise.
///
/// The distinction matters: naming a process that is merely open records
/// silence, and that is discovered after the meeting.
func listAudioApps() -> [AudioApp] {
    let objects = audioObjectArray(AudioObjectID(kAudioObjectSystemObject),
                                   kAudioHardwarePropertyProcessObjectList,
                                   AudioObjectID(0))
    var apps: [AudioApp] = []
    for obj in objects {
        let (pid, pidStatus) = audioObjectProperty(obj, kAudioProcessPropertyPID,
                                                   kAudioObjectPropertyScopeGlobal, pid_t(0))
        if pidStatus != noErr || pid <= 0 { continue }
        let (running, _) = audioObjectProperty(obj, kAudioProcessPropertyIsRunningOutput,
                                               kAudioObjectPropertyScopeGlobal, UInt32(0))
        // A blank bundle ID is common for helpers and command line tools, and
        // an empty name in the listing is useless to whoever is trying to find
        // their meeting in it.
        var name = audioObjectString(obj, kAudioProcessPropertyBundleID) ?? ""
        if name.isEmpty { name = processName(pid) ?? "" }
        if name.isEmpty { name = "pid \(pid)" }
        apps.append(AudioApp(pid: Int(pid), name: name, active: running != 0))
    }
    return apps
}

private func processName(_ pid: pid_t) -> String? {
    var buf = [CChar](repeating: 0, count: 4096)
    let n = proc_pidpath(pid, &buf, UInt32(buf.count))
    guard n > 0 else { return nil }
    let path = String(cString: buf)
    return (path as NSString).lastPathComponent
}
