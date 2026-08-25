// minutes capture helper — Windows.
//
// Captures two tracks and writes them to stdout as framed, timestamped chunks:
// track 0 is the default capture endpoint (the microphone), track 1 is the
// default render endpoint opened in loopback mode (everything the machine is
// playing). The two are never mixed. See docs/protocol.md for the frame layout.
//
// Loopback, deliberately, and not a virtual audio cable: routing playback
// through VB-Cable or Voicemeeter also works, but its failure mode is the bad
// one — the recording succeeds while the human stops hearing the meeting.
// WASAPI loopback observes the render endpoint and leaves playback untouched.

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <mmdeviceapi.h>
#include <audioclient.h>
#include <functiondiscoverykeys_devpkey.h>
#include <mmreg.h>

#include <stdio.h>
#include <stdint.h>
#include <stdlib.h>
#include <stdarg.h>
#include <string.h>
#include <io.h>
#include <fcntl.h>

#include <atomic>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

// ---------------------------------------------------------------- protocol --

static const uint32_t kMagic = 0x314E494DUL; // "MIN1"

enum FrameType : uint16_t {
    FRAME_TRACK_INFO = 1,
    FRAME_AUDIO      = 2,
    FRAME_END        = 3,
    FRAME_LOG        = 4,
};

enum TrackID : uint16_t {
    TRACK_MIC    = 0,
    TRACK_SYSTEM = 1,
};

enum FrameFlags : uint32_t {
    FLAG_SILENT          = 1u << 0,
    FLAG_DISCONTINUITY   = 1u << 1,
    FLAG_TIMESTAMP_ERROR = 1u << 2,
};

// One lock over stdout. Two capture threads share it, and a frame's header and
// payload must not be split by the other thread's write.
static std::mutex g_stdoutMu;

static void put32(uint8_t* p, uint32_t v) {
    p[0] = (uint8_t)(v); p[1] = (uint8_t)(v >> 8);
    p[2] = (uint8_t)(v >> 16); p[3] = (uint8_t)(v >> 24);
}
static void put16(uint8_t* p, uint16_t v) {
    p[0] = (uint8_t)(v); p[1] = (uint8_t)(v >> 8);
}
static void put64(uint8_t* p, uint64_t v) {
    for (int i = 0; i < 8; ++i) p[i] = (uint8_t)(v >> (8 * i));
}

static void writeFrame(uint16_t type, uint16_t track, uint64_t qpc100ns,
                       uint64_t devicePos, uint32_t flags,
                       const void* payload, uint32_t payloadLen) {
    uint8_t h[32];
    put32(h + 0,  kMagic);
    put16(h + 4,  type);
    put16(h + 6,  track);
    put64(h + 8,  qpc100ns);
    put64(h + 16, devicePos);
    put32(h + 24, payloadLen);
    put32(h + 28, flags);

    std::lock_guard<std::mutex> lock(g_stdoutMu);
    fwrite(h, 1, sizeof(h), stdout);
    if (payloadLen) fwrite(payload, 1, payloadLen, stdout);
    fflush(stdout);
}

static void logf(uint16_t track, const char* fmt, ...) {
    char buf[1024];
    va_list ap;
    va_start(ap, fmt);
    int n = vsnprintf(buf, sizeof(buf), fmt, ap);
    va_end(ap);
    if (n < 0) return;
    if (n > (int)sizeof(buf)) n = (int)sizeof(buf);
    writeFrame(FRAME_LOG, track, 0, 0, 0, buf, (uint32_t)n);
}

// ------------------------------------------------------------------ helpers --

static std::atomic<bool> g_stop(false);

// The mix format may be WAVE_FORMAT_EXTENSIBLE, whose real type lives in a
// subformat GUID. Those GUIDs are {0000000X-0000-0010-8000-00aa00389b71} where
// X is the equivalent format tag, so Data1 is the tag — which avoids linking
// the ksmedia GUIDs for a one-field read.
static uint16_t formatTagOf(const WAVEFORMATEX* w) {
    if (w->wFormatTag == WAVE_FORMAT_EXTENSIBLE) {
        const WAVEFORMATEXTENSIBLE* we = (const WAVEFORMATEXTENSIBLE*)w;
        return (uint16_t)we->SubFormat.Data1;
    }
    return w->wFormatTag;
}

static std::string endpointName(IMMDevice* dev) {
    IPropertyStore* props = NULL;
    if (FAILED(dev->OpenPropertyStore(STGM_READ, &props))) return "unknown";
    PROPVARIANT v;
    PropVariantInit(&v);
    std::string out = "unknown";
    if (SUCCEEDED(props->GetValue(PKEY_Device_FriendlyName, &v)) && v.vt == VT_LPWSTR) {
        int n = WideCharToMultiByte(CP_UTF8, 0, v.pwszVal, -1, NULL, 0, NULL, NULL);
        if (n > 1) {
            out.resize(n - 1);
            WideCharToMultiByte(CP_UTF8, 0, v.pwszVal, -1, &out[0], n, NULL, NULL);
        }
    }
    PropVariantClear(&v);
    props->Release();
    return out;
}

struct Endpoint {
    IMMDevice*          dev     = NULL;
    IAudioClient*       client  = NULL;
    IAudioCaptureClient* capture = NULL;
    WAVEFORMATEX*       fmt     = NULL;
    std::string         name;

    void release() {
        if (capture) { capture->Release(); capture = NULL; }
        if (client)  { client->Release();  client  = NULL; }
        if (dev)     { dev->Release();     dev     = NULL; }
        if (fmt)     { CoTaskMemFree(fmt); fmt     = NULL; }
    }
};

// Opens the default endpoint for a track and initialises it for capture.
// `loopback` selects the render endpoint observed in loopback mode.
static HRESULT openEndpoint(bool loopback, Endpoint* ep, std::string* err) {
    IMMDeviceEnumerator* enumr = NULL;
    HRESULT hr = CoCreateInstance(__uuidof(MMDeviceEnumerator), NULL, CLSCTX_ALL,
                                  __uuidof(IMMDeviceEnumerator), (void**)&enumr);
    if (FAILED(hr)) { *err = "CoCreateInstance(MMDeviceEnumerator)"; return hr; }

    hr = enumr->GetDefaultAudioEndpoint(loopback ? eRender : eCapture, eConsole, &ep->dev);
    enumr->Release();
    if (FAILED(hr)) {
        *err = loopback ? "no default render endpoint (nothing to capture system audio from)"
                        : "no default capture endpoint (no microphone)";
        return hr;
    }

    ep->name = endpointName(ep->dev);

    hr = ep->dev->Activate(__uuidof(IAudioClient), CLSCTX_ALL, NULL, (void**)&ep->client);
    if (FAILED(hr)) { *err = "IMMDevice::Activate(IAudioClient)"; return hr; }

    hr = ep->client->GetMixFormat(&ep->fmt);
    if (FAILED(hr)) { *err = "IAudioClient::GetMixFormat"; return hr; }

    // 200ms of buffer. Shared mode, so the endpoint keeps its own period; this
    // only sets how much slack there is before a slow reader loses data.
    const REFERENCE_TIME hnsBuffer = 2000000;
    DWORD streamFlags = loopback ? AUDCLNT_STREAMFLAGS_LOOPBACK : 0;
    hr = ep->client->Initialize(AUDCLNT_SHAREMODE_SHARED, streamFlags,
                                hnsBuffer, 0, ep->fmt, NULL);
    if (FAILED(hr)) { *err = "IAudioClient::Initialize"; return hr; }

    hr = ep->client->GetService(__uuidof(IAudioCaptureClient), (void**)&ep->capture);
    if (FAILED(hr)) { *err = "IAudioClient::GetService(IAudioCaptureClient)"; return hr; }

    return S_OK;
}

// --------------------------------------------------------------- capturing --

struct TrackResult {
    std::atomic<bool>    started{false};
    std::atomic<bool>    failed{false};
    std::atomic<uint64_t> framesCaptured{0};
    std::string          error;
};

// describeHR names the failures worth recognising by sight.
//
// AUDCLNT_E_DEVICE_INVALIDATED is the one that matters: it is what a default
// endpoint change, an unplugged headset or a removed device produces. Left
// unnamed it is an eight-digit number in a log; named, it is the reason the
// meeting stopped.
static const char* describeHR(HRESULT hr) {
    switch (hr) {
    case AUDCLNT_E_DEVICE_INVALIDATED:
        return " (the audio device was removed, disabled, or the default endpoint changed)";
    case AUDCLNT_E_SERVICE_NOT_RUNNING:
        return " (the Windows audio service stopped)";
    case AUDCLNT_E_RESOURCES_INVALIDATED:
        return " (the audio session was taken away)";
    default:
        return "";
    }
}

// failMidStream records that a running capture died.
//
// Distinct from failing to start, and it must be, because the two look
// identical from outside unless they are separated here: a recording that
// stopped early because the headphones came out would otherwise be
// indistinguishable from one somebody chose to stop. That is a meeting cut in
// half reported as a meeting that ended.
static void failMidStream(TrackResult* result, uint16_t trackID, const char* what, HRESULT hr) {
    char msg[512];
    snprintf(msg, sizeof(msg), "%s failed mid-recording: 0x%08lX%s",
             what, (unsigned long)hr, describeHR(hr));
    result->error = msg;
    result->failed = true;
    logf(trackID, "%s", msg);
}

static void captureTrack(uint16_t trackID, bool loopback, TrackResult* result) {
    HRESULT hr = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    bool comInit = SUCCEEDED(hr);

    Endpoint ep;
    std::string err;
    hr = openEndpoint(loopback, &ep, &err);
    if (FAILED(hr)) {
        char msg[512];
        snprintf(msg, sizeof(msg), "%s failed: 0x%08lX", err.c_str(), (unsigned long)hr);
        result->error = msg;
        result->failed = true;
        logf(trackID, "%s", msg);
        ep.release();
        if (comInit) CoUninitialize();
        return;
    }

    LARGE_INTEGER qpcFreq;
    QueryPerformanceFrequency(&qpcFreq);

    // TRACK_INFO before any audio, so a reader never sees samples it cannot
    // interpret.
    {
        std::vector<uint8_t> info(24 + ep.name.size());
        put32(&info[0],  ep.fmt->nSamplesPerSec);
        put16(&info[4],  ep.fmt->nChannels);
        put16(&info[6],  ep.fmt->wBitsPerSample);
        put16(&info[8],  formatTagOf(ep.fmt));
        put16(&info[10], ep.fmt->nBlockAlign);
        put64(&info[12], (uint64_t)qpcFreq.QuadPart);
        put32(&info[20], (uint32_t)ep.name.size());
        if (!ep.name.empty()) memcpy(&info[24], ep.name.data(), ep.name.size());
        writeFrame(FRAME_TRACK_INFO, trackID, 0, 0, 0, info.data(), (uint32_t)info.size());
    }

    hr = ep.client->Start();
    if (FAILED(hr)) {
        char msg[256];
        snprintf(msg, sizeof(msg), "IAudioClient::Start failed: 0x%08lX", (unsigned long)hr);
        result->error = msg;
        result->failed = true;
        logf(trackID, "%s", msg);
        ep.release();
        if (comInit) CoUninitialize();
        return;
    }
    result->started = true;

    // Polled rather than event-driven: loopback streams do not reliably signal
    // an event handle, and one loop that works for both endpoints is worth more
    // than the few milliseconds of latency a 10ms poll costs a recorder.
    std::vector<uint8_t> silence;
    while (!g_stop.load()) {
        UINT32 packetFrames = 0;
        hr = ep.capture->GetNextPacketSize(&packetFrames);
        if (FAILED(hr)) {
            failMidStream(result, trackID, "GetNextPacketSize", hr);
            break;
        }
        if (packetFrames == 0) {
            Sleep(10);
            continue;
        }
        while (packetFrames > 0 && !g_stop.load()) {
            BYTE*  data      = NULL;
            UINT32 numFrames = 0;
            DWORD  wasapiFlags = 0;
            UINT64 devicePos = 0, qpcPos = 0;

            hr = ep.capture->GetBuffer(&data, &numFrames, &wasapiFlags, &devicePos, &qpcPos);
            if (hr == AUDCLNT_S_BUFFER_EMPTY) break;
            if (FAILED(hr)) {
                failMidStream(result, trackID, "GetBuffer", hr);
                g_stop = true;
                break;
            }

            uint32_t flags = 0;
            if (wasapiFlags & AUDCLNT_BUFFERFLAGS_SILENT)               flags |= FLAG_SILENT;
            if (wasapiFlags & AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY)   flags |= FLAG_DISCONTINUITY;
            if (wasapiFlags & AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR)      flags |= FLAG_TIMESTAMP_ERROR;

            const uint32_t bytes = numFrames * ep.fmt->nBlockAlign;

            // A silent packet's buffer contents are undefined, so write real
            // zeroes. Keeping the stream dense means downstream never has to
            // decide whether a missing packet was silence or a dropout.
            if (flags & FLAG_SILENT) {
                if (silence.size() < bytes) silence.assign(bytes, 0);
                writeFrame(FRAME_AUDIO, trackID, qpcPos, devicePos, flags, silence.data(), bytes);
            } else {
                writeFrame(FRAME_AUDIO, trackID, qpcPos, devicePos, flags, data, bytes);
            }
            result->framesCaptured += numFrames;

            hr = ep.capture->ReleaseBuffer(numFrames);
            if (FAILED(hr)) {
                failMidStream(result, trackID, "ReleaseBuffer", hr);
                g_stop = true;
                break;
            }

            hr = ep.capture->GetNextPacketSize(&packetFrames);
            if (FAILED(hr)) {
                failMidStream(result, trackID, "GetNextPacketSize", hr);
                g_stop = true;
                break;
            }
        }
    }

    ep.client->Stop();
    writeFrame(FRAME_END, trackID, 0, result->framesCaptured.load(), 0, NULL, 0);
    ep.release();
    if (comInit) CoUninitialize();
}

// --------------------------------------------------------------- preflight --

// Answers one question — would a recording started now contain both sides of
// the meeting — by actually opening both endpoints rather than reasoning about
// them. A device that enumerates but refuses to initialise is exactly the
// failure this is here to catch, and only opening it finds that out.
static int preflight() {
    HRESULT hr = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    bool comInit = SUCCEEDED(hr);

    struct Probe { const char* key; bool loopback; };
    Probe probes[2] = { {"microphone", false}, {"system", true} };

    printf("{\n  \"platform\": \"windows\",\n  \"tracks\": {\n");
    bool allOK = true;
    for (int i = 0; i < 2; ++i) {
        Endpoint ep;
        std::string err;
        HRESULT phr = openEndpoint(probes[i].loopback, &ep, &err);
        bool ok = SUCCEEDED(phr);
        if (ok) {
            // Initialising is not proof it will run; starting it is.
            phr = ep.client->Start();
            ok = SUCCEEDED(phr);
            if (ok) ep.client->Stop();
            else    err = "IAudioClient::Start";
        }
        if (!ok) allOK = false;

        printf("    \"%s\": {\n", probes[i].key);
        printf("      \"ok\": %s,\n", ok ? "true" : "false");
        printf("      \"mode\": \"%s\",\n", probes[i].loopback ? "wasapi-loopback" : "wasapi-capture");
        if (ok) {
            printf("      \"device\": \"%s\",\n", ep.name.c_str());
            printf("      \"sampleRate\": %lu,\n", (unsigned long)ep.fmt->nSamplesPerSec);
            printf("      \"channels\": %u,\n", (unsigned)ep.fmt->nChannels);
            printf("      \"bitsPerSample\": %u,\n", (unsigned)ep.fmt->wBitsPerSample);
            printf("      \"formatTag\": %u\n", (unsigned)formatTagOf(ep.fmt));
        } else {
            printf("      \"error\": \"%s\",\n", err.c_str());
            printf("      \"hresult\": \"0x%08lX\"\n", (unsigned long)phr);
        }
        printf("    }%s\n", i == 0 ? "," : "");
        ep.release();
    }
    printf("  },\n  \"ok\": %s\n}\n", allOK ? "true" : "false");

    if (comInit) CoUninitialize();
    return allOK ? 0 : 1;
}

// -------------------------------------------------------------------- main --

static BOOL WINAPI ctrlHandler(DWORD) {
    g_stop = true;
    return TRUE;
}

// Stops when the parent closes stdin. The orchestrator is a Linux process
// talking to this one over WSL interop, and closing a pipe is the one stop
// signal that crosses that boundary without a control channel.
static void watchStdin() {
    char b;
    for (;;) {
        int n = _read(0, &b, 1);
        if (n <= 0) break;   // EOF or error: parent is done with us
        if (b == 'q') break;
    }
    g_stop = true;
}

static void usage() {
    fprintf(stderr,
        "minutes capture helper\n"
        "  --preflight        report whether both tracks can be captured, as JSON\n"
        "  --duration-ms N    stop after N milliseconds (default: run until stdin closes)\n"
        "  --mic-only         capture track 0 only\n"
        "  --system-only      capture track 1 only\n");
}

int main(int argc, char** argv) {
    bool doPreflight = false, micOnly = false, systemOnly = false;
    long durationMs = 0;

    for (int i = 1; i < argc; ++i) {
        std::string a = argv[i];
        if (a == "--preflight")        doPreflight = true;
        else if (a == "--mic-only")    micOnly = true;
        else if (a == "--system-only") systemOnly = true;
        else if (a == "--duration-ms" && i + 1 < argc) durationMs = atol(argv[++i]);
        else if (a == "--help" || a == "-h") { usage(); return 0; }
        else { fprintf(stderr, "unknown argument: %s\n", a.c_str()); usage(); return 2; }
    }

    if (doPreflight) {
        // Text on stdout in this mode, so leave it in text mode.
        return preflight();
    }

    // Everything after this point is binary. Without this, the CRT rewrites
    // 0x0A to 0x0D 0x0A and the audio arrives corrupted in a way that still
    // plays.
    if (_setmode(_fileno(stdout), _O_BINARY) == -1) {
        fprintf(stderr, "failed to set stdout to binary mode\n");
        return 3;
    }
    setvbuf(stdout, NULL, _IOFBF, 1 << 16);

    SetConsoleCtrlHandler(ctrlHandler, TRUE);

    TrackResult micResult, sysResult;
    std::vector<std::thread> threads;
    if (!systemOnly) threads.emplace_back(captureTrack, TRACK_MIC,    false, &micResult);
    if (!micOnly)    threads.emplace_back(captureTrack, TRACK_SYSTEM, true,  &sysResult);

    std::thread stdinWatcher(watchStdin);
    stdinWatcher.detach();

    if (durationMs > 0) {
        // Against the clock, not an accumulation of Sleep() calls. The default
        // timer granularity is 15.6ms, so Sleep(50) actually returns after
        // about 62.5 — summing those overshoots by a quarter, which at meeting
        // length is minutes.
        const ULONGLONG deadline = GetTickCount64() + (ULONGLONG)durationMs;
        while (!g_stop.load() && GetTickCount64() < deadline)
            Sleep(10);
        g_stop = true;
    }

    for (auto& t : threads) t.join();
    fflush(stdout);

    // A track that never started, or that died while running, is a half-recorded
    // meeting. Both are worth a non-zero exit rather than a file nobody checks.
    //
    // The second case used to exit zero, which meant a meeting cut in half by an
    // unplugged headset was recorded as a meeting that ended there.
    if ((!systemOnly && micResult.failed.load()) || (!micOnly && sysResult.failed.load())) {
        if (!micResult.error.empty())
            fprintf(stderr, "mic track: %s\n", micResult.error.c_str());
        if (!sysResult.error.empty())
            fprintf(stderr, "system track: %s\n", sysResult.error.c_str());
        return 1;
    }
    return 0;
}
