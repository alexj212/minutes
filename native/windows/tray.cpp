// minutes-tray — the disclosure that a recording is happening, on screen.
//
// A marker file and a notification both help, and neither is visible while the
// meeting is in progress. This project argues that an active recording should
// be obvious rather than quiet; a tray icon is the only form of that which is
// continuously true rather than periodically asserted.
//
// It is a separate binary from the capture helper on purpose. A GUI message
// loop and two real-time audio threads in one process is a way to drop packets
// for a reason nobody will ever find, and the tray dying must not take the
// recording with it.
//
// Lifecycle is the same contract as the capture helper, for the same reasons:
// the orchestrator closes stdin to ask it to go, and stdin closing when the
// orchestrator dies means nothing is ever orphaned. It is started in its own
// process group, so a Ctrl-C in the terminal does not reach it.
//
// It talks back on stdout, one line:
//
//   STOP    the operator asked for the recording to stop
//
// Nothing else. Deciding to stop is the operator's; carrying that out belongs
// to the orchestrator, which owns the recording and the manifest. A tray icon
// that killed the capture helper itself would produce exactly the half-written
// recording the rest of this program is built to avoid.

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <shellapi.h>
#include <stdio.h>
#include <string>
#include <vector>

#pragma comment(lib, "shell32.lib")
#pragma comment(lib, "user32.lib")
#pragma comment(lib, "gdi32.lib")

static const UINT WM_TRAY = WM_APP + 1;
static const UINT ID_STOP = 1001;
static const UINT ID_FOLDER = 1002;
static const UINT ID_ELAPSED = 1003;
static const UINT TIMER_TICK = 1;

static NOTIFYICONDATAW g_nid = {};
static HWND g_hwnd = nullptr;
static std::wstring g_name;
static std::wstring g_dir;
static ULONGLONG g_startedMs = 0;
static bool g_stopping = false;

// say writes one line to stdout and flushes.
//
// Unbuffered on purpose. The orchestrator is reading this pipe to learn that a
// human asked for the recording to end, and a message sitting in a buffer until
// the process exits arrives after the thing it was asking for.
static void say(const char *line) {
    fputs(line, stdout);
    fputc('\n', stdout);
    fflush(stdout);
}

static std::wstring elapsedText() {
    ULONGLONG secs = (GetTickCount64() - g_startedMs) / 1000;
    wchar_t buf[64];
    if (secs < 3600) {
        swprintf(buf, 64, L"recording — %llum%02llus", secs / 60, secs % 60);
    } else {
        swprintf(buf, 64, L"recording — %lluh%02llum", secs / 3600, (secs % 3600) / 60);
    }
    return buf;
}

// tooltip is what hovering the icon says. Deliberately the meeting's name and
// the elapsed time rather than "minutes": the question somebody hovers to ask
// is "what is being recorded and for how long", not "what program is this".
static void updateTooltip() {
    std::wstring s = g_name.empty() ? L"minutes" : g_name;
    s += L"\n";
    s += elapsedText();
    if (s.size() > 127) s.resize(127);
    wcsncpy_s(g_nid.szTip, s.c_str(), _TRUNCATE);
    Shell_NotifyIconW(NIM_MODIFY, &g_nid);
}

// makeIcon draws a red dot, which is what a recording indicator looks like
// everywhere else. Drawn rather than shipped as a resource so the helper stays
// one self-contained .exe with no files beside it to lose.
static HICON makeIcon() {
    int n = GetSystemMetrics(SM_CXSMICON);
    if (n <= 0) n = 16;
    HDC screen = GetDC(nullptr);
    HDC dc = CreateCompatibleDC(screen);
    HBITMAP color = CreateCompatibleBitmap(screen, n, n);
    HBITMAP mask = CreateBitmap(n, n, 1, 1, nullptr);
    HGDIOBJ old = SelectObject(dc, color);

    RECT all = {0, 0, n, n};
    HBRUSH black = CreateSolidBrush(RGB(0, 0, 0));
    FillRect(dc, &all, black);
    DeleteObject(black);

    HBRUSH red = CreateSolidBrush(RGB(220, 40, 40));
    HGDIOBJ oldBrush = SelectObject(dc, red);
    HGDIOBJ oldPen = SelectObject(dc, GetStockObject(NULL_PEN));
    Ellipse(dc, 1, 1, n - 1, n - 1);
    SelectObject(dc, oldPen);
    SelectObject(dc, oldBrush);
    DeleteObject(red);

    // The mask is the inverse: opaque where the dot is drawn.
    SelectObject(dc, mask);
    RECT m = {0, 0, n, n};
    HBRUSH white = CreateSolidBrush(RGB(255, 255, 255));
    FillRect(dc, &m, white);
    DeleteObject(white);
    HBRUSH blackBrush = CreateSolidBrush(RGB(0, 0, 0));
    oldBrush = SelectObject(dc, blackBrush);
    oldPen = SelectObject(dc, GetStockObject(NULL_PEN));
    Ellipse(dc, 1, 1, n - 1, n - 1);
    SelectObject(dc, oldPen);
    SelectObject(dc, oldBrush);
    DeleteObject(blackBrush);

    SelectObject(dc, old);
    DeleteDC(dc);
    ReleaseDC(nullptr, screen);

    ICONINFO ii = {};
    ii.fIcon = TRUE;
    ii.hbmMask = mask;
    ii.hbmColor = color;
    HICON icon = CreateIconIndirect(&ii);
    DeleteObject(color);
    DeleteObject(mask);
    return icon;
}

static void showMenu() {
    HMENU menu = CreatePopupMenu();
    if (!menu) return;

    std::wstring header = g_name.empty() ? L"(unnamed recording)" : g_name;
    AppendMenuW(menu, MF_STRING | MF_DISABLED | MF_GRAYED, 0, header.c_str());
    AppendMenuW(menu, MF_STRING | MF_DISABLED | MF_GRAYED, ID_ELAPSED, elapsedText().c_str());
    AppendMenuW(menu, MF_SEPARATOR, 0, nullptr);
    AppendMenuW(menu, MF_STRING, ID_STOP,
                g_stopping ? L"Stopping…" : L"Stop recording");
    if (!g_dir.empty()) {
        AppendMenuW(menu, MF_STRING, ID_FOLDER, L"Open folder");
    }
    if (g_stopping) {
        EnableMenuItem(menu, ID_STOP, MF_BYCOMMAND | MF_GRAYED);
    }

    POINT pt;
    GetCursorPos(&pt);
    // Required, and the reason is not obvious: without it the menu does not
    // dismiss when the user clicks elsewhere, and stays on screen over
    // everything.
    SetForegroundWindow(g_hwnd);
    TrackPopupMenu(menu, TPM_RIGHTBUTTON | TPM_BOTTOMALIGN, pt.x, pt.y, 0, g_hwnd, nullptr);
    PostMessageW(g_hwnd, WM_NULL, 0, 0);
    DestroyMenu(menu);
}

static void askToStop() {
    if (g_stopping) return;
    g_stopping = true;
    say("STOP");
    // The icon stays until the orchestrator closes stdin. It is still
    // recording until then — the request has been made, not carried out — and
    // an indicator that vanishes on the request would be claiming otherwise.
    updateTooltip();
}

static LRESULT CALLBACK wndProc(HWND hwnd, UINT msg, WPARAM wp, LPARAM lp) {
    switch (msg) {
    case WM_TRAY:
        if (LOWORD(lp) == WM_RBUTTONUP || LOWORD(lp) == WM_LBUTTONUP ||
            LOWORD(lp) == WM_CONTEXTMENU) {
            showMenu();
        }
        return 0;
    case WM_COMMAND:
        switch (LOWORD(wp)) {
        case ID_STOP:
            askToStop();
            return 0;
        case ID_FOLDER:
            if (!g_dir.empty()) {
                ShellExecuteW(nullptr, L"open", g_dir.c_str(), nullptr, nullptr, SW_SHOWNORMAL);
            }
            return 0;
        }
        return 0;
    case WM_TIMER:
        if (wp == TIMER_TICK) updateTooltip();
        return 0;
    case WM_DESTROY:
        PostQuitMessage(0);
        return 0;
    }
    return DefWindowProcW(hwnd, msg, wp, lp);
}

// watchStdin posts a quit when the orchestrator closes the pipe.
//
// The message loop cannot poll for this, so it is read on its own thread. EOF
// is the only shutdown signal this program has: nothing else is allowed to
// reach it, and the pipe closes on its own if the orchestrator dies, so it can
// never be left running with no recording behind it.
static DWORD WINAPI watchStdin(LPVOID) {
    char buf[64];
    while (fgets(buf, sizeof buf, stdin) != nullptr) {
        // Nothing is sent this way today. Reading and discarding means a future
        // message does not wedge the orchestrator writing into a full pipe.
    }
    PostMessageW(g_hwnd, WM_CLOSE, 0, 0);
    return 0;
}

static std::wstring widen(const char *s) {
    if (!s) return L"";
    int n = MultiByteToWideChar(CP_UTF8, 0, s, -1, nullptr, 0);
    if (n <= 0) return L"";
    std::wstring out(n - 1, L'\0');
    MultiByteToWideChar(CP_UTF8, 0, s, -1, &out[0], n);
    return out;
}

int main(int argc, char **argv) {
    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "--name") && i + 1 < argc) {
            g_name = widen(argv[++i]);
        } else if (!strcmp(argv[i], "--dir") && i + 1 < argc) {
            g_dir = widen(argv[++i]);
        }
    }
    g_startedMs = GetTickCount64();

    HINSTANCE inst = GetModuleHandleW(nullptr);
    WNDCLASSEXW wc = {};
    wc.cbSize = sizeof wc;
    wc.lpfnWndProc = wndProc;
    wc.hInstance = inst;
    wc.lpszClassName = L"MinutesTrayWindow";
    if (!RegisterClassExW(&wc)) {
        fprintf(stderr, "tray: RegisterClassEx failed: %lu\n", GetLastError());
        return 1;
    }
    // A message-only window: it never appears, and exists so the shell has
    // somewhere to deliver icon notifications.
    g_hwnd = CreateWindowExW(0, wc.lpszClassName, L"minutes", 0, 0, 0, 0, 0,
                             HWND_MESSAGE, nullptr, inst, nullptr);
    if (!g_hwnd) {
        fprintf(stderr, "tray: CreateWindowEx failed: %lu\n", GetLastError());
        return 1;
    }

    g_nid.cbSize = sizeof g_nid;
    g_nid.hWnd = g_hwnd;
    g_nid.uID = 1;
    g_nid.uFlags = NIF_ICON | NIF_MESSAGE | NIF_TIP;
    g_nid.uCallbackMessage = WM_TRAY;
    g_nid.hIcon = makeIcon();
    wcsncpy_s(g_nid.szTip, L"minutes", _TRUNCATE);
    if (!Shell_NotifyIconW(NIM_ADD, &g_nid)) {
        // Reported rather than swallowed: the orchestrator says the indicator
        // is up, and it must not say that when it is not. The recording is not
        // affected either way, which is why this exits rather than aborting
        // anything.
        fprintf(stderr, "tray: Shell_NotifyIcon(NIM_ADD) failed: %lu\n", GetLastError());
        return 1;
    }
    updateTooltip();
    SetTimer(g_hwnd, TIMER_TICK, 1000, nullptr);
    say("READY");

    CreateThread(nullptr, 0, watchStdin, nullptr, 0, nullptr);

    MSG msg;
    while (GetMessageW(&msg, nullptr, 0, 0) > 0) {
        TranslateMessage(&msg);
        DispatchMessageW(&msg);
    }

    Shell_NotifyIconW(NIM_DELETE, &g_nid);
    if (g_nid.hIcon) DestroyIcon(g_nid.hIcon);
    return 0;
}
