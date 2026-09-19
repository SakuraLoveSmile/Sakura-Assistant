package com.sakurasep.assistant

import android.app.Notification
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.ConnectivityManager
import android.net.Network
import android.os.IBinder
import androidx.core.app.ServiceCompat
import com.sakurasep.assistant.data.ConfigEntity
import com.sakurasep.assistant.data.ConfigStore
import com.sakurasep.assistant.data.HeldEntity
import com.sakurasep.assistant.data.NativeDb
import com.sakurasep.assistant.data.NotifiedEntity
import com.sakurasep.assistant.dnd.DndWindow
import com.sakurasep.assistant.bridge.NativeEvents
import com.sakurasep.assistant.notify.FaultDebounce
import com.sakurasep.assistant.notify.NotificationHelper
import com.sakurasep.assistant.sse.SseParser
import com.sakurasep.assistant.util.Dbg
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import java.util.TimeZone
import java.util.concurrent.TimeUnit
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull
import okhttp3.Call
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject

/**
 * 常驻前台同步服务（bridge.md §3/§5）：
 * OkHttp SSE 读 /api/v1/stream?since=，断线指数退避 1s→60s，网络恢复立即重试；
 * notify.kind 驱动本地通知；DND 本地判定 + 窗口结束汇总；同 faultId 60s≤3 次去抖。
 */
class AssistantSyncService : Service() {

    companion object {
        const val ACTION_START = "com.sakurasep.assistant.action.START"
        const val ACTION_STOP = "com.sakurasep.assistant.action.STOP"
        const val ACTION_RECONNECT = "com.sakurasep.assistant.action.RECONNECT"

        fun start(context: Context) {
            val i = Intent(context, AssistantSyncService::class.java).setAction(ACTION_START)
            androidx.core.content.ContextCompat.startForegroundService(context, i)
        }

        fun requestStop(context: Context) {
            val i = Intent(context, AssistantSyncService::class.java).setAction(ACTION_STOP)
            context.startService(i)
        }

        fun requestReconnect(context: Context) {
            val i = Intent(context, AssistantSyncService::class.java).setAction(ACTION_RECONNECT)
            context.startService(i)
        }
    }

    private lateinit var db: NativeDb
    private lateinit var configStore: ConfigStore
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private var sseJob: Job? = null
    private var tickerJob: Job? = null
    private val debounce = FaultDebounce()

    /** 指数退避等待 / 网络恢复立即重试的信号。 */
    private val reconnectSignal = Channel<Unit>(Channel.CONFLATED)
    /** DND 立即重算的 poke（configure 变更时）。 */
    private val dndPoke = Channel<Unit>(Channel.CONFLATED)

    @Volatile private var activeCall: Call? = null
    @Volatile private var stopping = false

    // 中枢专用客户端直连：不走系统 HTTP 代理（Wi-Fi 代理会把指向 LAN/loopback
    // 的中枢流量错误转发到代理自身的回环，且 Dart 层也不经代理——两层须一致）。
    private val httpClient = OkHttpClient.Builder()
        .proxy(java.net.Proxy.NO_PROXY)
        .connectTimeout(15, TimeUnit.SECONDS)
        .readTimeout(0, TimeUnit.MILLISECONDS) // SSE 长连接不设读超时
        .writeTimeout(15, TimeUnit.SECONDS)
        .retryOnConnectionFailure(false)
        .build()

    private val restClient = httpClient.newBuilder()
        .readTimeout(30, TimeUnit.SECONDS)
        .build()

    private val clockFmt = SimpleDateFormat("HH:mm:ss", Locale.getDefault()).apply {
        timeZone = TimeZone.getDefault()
    }

    private val networkCallback = object : ConnectivityManager.NetworkCallback() {
        override fun onAvailable(network: Network) {
            Dbg.i("network available → immediate SSE retry")
            reconnectSignal.trySend(Unit)
        }
    }

    override fun onCreate() {
        super.onCreate()
        db = NativeDb.get(this)
        configStore = ConfigStore(this)
        NotificationHelper.ensureChannels(this)
        runCatching {
            getSystemService(ConnectivityManager::class.java)
                ?.registerDefaultNetworkCallback(networkCallback)
        }
        Dbg.i("AssistantSyncService onCreate")
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                Dbg.i("service stop requested")
                shutdown()
                return START_NOT_STICKY
            }
            ACTION_RECONNECT -> {
                Dbg.i("reconnect requested")
                reconnectSignal.trySend(Unit)
                dndPoke.trySend(Unit)
                activeCall?.cancel()
            }
            else -> {
                // ACTION_START / null（START_STICKY 重启）
                ensureRunning()
            }
        }
        return START_STICKY
    }

    override fun onTaskRemoved(rootIntent: Intent?) {
        // 划掉最近任务：记录证据（真机验证项②）；服务靠 START_STICKY + WorkManager 自愈。
        Dbg.i("onTaskRemoved: task swiped away; service continues via FGS")
        super.onTaskRemoved(rootIntent)
    }

    private fun ensureRunning() {
        stopping = false
        // 先上常驻通知（必须在超时前调 startForeground）
        val notif = NotificationHelper.buildServiceNotification(this, serviceSubtitle())
        ServiceCompat.startForeground(
            this, NotificationHelper.ID_SERVICE, notif,
            ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC,
        )
        SyncState.running = true
        scope.launch { emitState() }
        startSseLoop()
        startTicker()
    }

    private fun shutdown() {
        stopping = true
        sseJob?.cancel()
        tickerJob?.cancel()
        activeCall?.cancel()
        SyncState.running = false
        SyncState.hubReachable = false
        scope.launch { emitState() }
        ServiceCompat.stopForeground(this, ServiceCompat.STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    override fun onDestroy() {
        runCatching {
            getSystemService(ConnectivityManager::class.java)
                ?.unregisterNetworkCallback(networkCallback)
        }
        if (!stopping) {
            // 系统杀服务（非用户 stop）：START_STICKY 会重启；这里仅记日志
            Dbg.w("service destroyed without stop request (system kill)")
        }
        SyncState.running = false
        SyncState.hubReachable = false
        scope.cancel()
        Dbg.i("AssistantSyncService onDestroy")
        super.onDestroy()
    }

    // ---------------- SSE 主循环 ----------------

    private fun startSseLoop() {
        if (sseJob?.isActive == true) return
        sseJob = scope.launch {
            var backoffMs = 1_000L
            while (isActive && !stopping) {
                val cfg = configStore.load()
                if (cfg.hubUrl.isBlank() || cfg.token.isBlank()) {
                    SyncState.hubReachable = false
                    SyncState.lastError = "not_configured"
                    updateServiceNotification()
                    emitState(cfg.lastChangeSeq)
                    // 未配置：每 30s 复查（configure 时会 poke reconnectSignal 立即生效）
                    withTimeoutOrNull(30_000) { reconnectSignal.receive() }
                    continue
                }
                try {
                    streamOnce(cfg)
                    backoffMs = 1_000L // 干净断开视为一次成功连接，退避归零
                } catch (ce: CancellationException) {
                    throw ce
                } catch (re: ResyncException) {
                    // 服务端要求 resync：已完成全量同步，立即重连，不计错误
                    backoffMs = 1_000L
                    continue
                } catch (ae: AuthException) {
                    // 401 自愈（bridge.md §3）：refresh 成功 → 立即重连；
                    // 失败（invalid_refresh / 无 refreshToken / 网络错）→ authExpired 置位，
                    // 30s 后复查（configure 写入新凭证时会 poke reconnectSignal 立即生效）
                    if (refreshAuth()) {
                        backoffMs = 1_000L
                        continue
                    }
                    emitState(cfg.lastChangeSeq)
                    withTimeoutOrNull(30_000) { reconnectSignal.receive() }
                    backoffMs = 1_000L
                    continue
                } catch (e: Exception) {
                    if (!isActive || stopping) break
                    SyncState.hubReachable = false
                    SyncState.lastError = e.javaClass.simpleName + ": " + (e.message ?: "")
                    Dbg.w("SSE stream error: ${SyncState.lastError}")
                    updateServiceNotification()
                    emitState(cfg.lastChangeSeq)
                }
                // 退避等待，或网络恢复立即重试
                withTimeoutOrNull(backoffMs) { reconnectSignal.receive() }
                backoffMs = (backoffMs * 2).coerceAtMost(60_000)
            }
        }
    }

    private class ResyncException : Exception("server requested resync")

    private class AuthException : Exception("HTTP 401 unauthorized")

    /**
     * 401 自愈（bridge.md §3）：本地 refreshToken 调 /api/v1/auth/refresh 一次性轮换。
     * 成功 → 更新 token/refreshToken 副本并返回 true（调用方立即重连）；
     * invalid_refresh / 无 refreshToken → authExpired 置位返回 false；
     * 网络错 / 其他 HTTP 错 → 返回 false 由调用方退避复查。
     */
    private suspend fun refreshAuth(): Boolean {
        val cfg = configStore.load()
        if (cfg.hubUrl.isBlank() || cfg.refreshToken.isBlank()) {
            SyncState.authExpired = true
            SyncState.lastError = "auth_expired"
            return false
        }
        val url = cfg.hubUrl.trimEnd('/') + "/api/v1/auth/refresh"
        val payload = JSONObject().put("refreshToken", cfg.refreshToken).toString()
        val req = Request.Builder().url(url)
            .post(payload.toRequestBody("application/json".toMediaType()))
            .build()
        val code = runCatching {
            restClient.newCall(req).execute().use { resp ->
                if (resp.code == 200) {
                    val json = JSONObject(resp.body?.string() ?: "{}")
                    val newToken = json.optString("token")
                    val newRefresh = json.optString("refreshToken")
                    if (newToken.isNotEmpty()) {
                        val appliedRefresh = newRefresh.ifEmpty { cfg.refreshToken }
                        configStore.update { c ->
                            c.copy(
                                token = newToken,
                                refreshToken = appliedRefresh,
                            )
                        }
                        // 旧 refreshToken 已随轮换失效：回写 Dart 侧会话，防双端凭证分裂。
                        NativeEvents.emitSession(newToken, appliedRefresh)
                        SyncState.authExpired = false
                        SyncState.lastError = null
                        Dbg.i("auth refresh ok → reconnect with rotated token")
                        return@use 0
                    }
                }
                resp.code
            }
        }.getOrElse { -1 }
        return when (code) {
            0 -> true
            401 -> {
                SyncState.authExpired = true
                SyncState.lastError = "auth_expired"
                updateServiceNotification()
                Dbg.w("auth refresh rejected (invalid_refresh) → waiting for re-login")
                false
            }
            else -> {
                Dbg.w("auth refresh failed http=$code")
                false
            }
        }
    }

    /** 一次 SSE 连接：读流直到断开 / 异常返回。 */
    private suspend fun streamOnce(cfg: ConfigEntity) {
        val url = cfg.hubUrl.trimEnd('/') + "/api/v1/stream?since=" + cfg.lastChangeSeq
        val req = Request.Builder()
            .url(url)
            .header("Authorization", "Bearer ${cfg.token}")
            .header("Accept", "text/event-stream")
            .header("Cache-Control", "no-cache")
            .build()
        val call = httpClient.newCall(req)
        activeCall = call
        Dbg.i("SSE connect $url")
        try {
            call.execute().use { resp ->
                when (resp.code) {
                    200 -> {
                        SyncState.hubReachable = true
                        SyncState.authExpired = false
                        SyncState.lastError = null
                        SyncState.lastSyncAtMs = System.currentTimeMillis()
                        updateServiceNotification()
                        emitState(cfg.lastChangeSeq)
                        Dbg.i("SSE connected (HTTP 200)")
                        val parser = SseParser()
                        val source = resp.body?.source()
                            ?: throw Exception("empty SSE body")
                        while (true) {
                            val line = source.readUtf8Line() ?: break // EOF
                            val ev = parser.feed(line) ?: continue
                            handleSseEvent(ev)
                        }
                        Dbg.i("SSE stream EOF")
                    }
                    401 -> {
                        SyncState.lastError = "unauthorized"
                        throw AuthException()
                    }
                    else -> throw Exception("HTTP ${resp.code}")
                }
            }
        } finally {
            activeCall = null
            if (SyncState.hubReachable) {
                SyncState.hubReachable = false
                updateServiceNotification()
                emitState()
            }
        }
    }

    private suspend fun handleSseEvent(ev: SseParser.Event) {
        Dbg.timed("sse event type=${ev.type} id=${ev.id} bytes=${ev.data.length}")
        // id = changeSeq：持久化续传游标
        ev.id?.toLongOrNull()?.let { seq ->
            configStore.update { c ->
                if (seq > c.lastChangeSeq) c.copy(lastChangeSeq = seq) else c
            }
        }
        when (ev.type) {
            "ping" -> touchSync()
            "message" -> onMessageEvent(ev.data)
            "fault" -> onFaultEvent(ev.data)
            "settings" -> onSettingsEvent(ev.data)
            "source", "rules" -> touchSync()
            "resync" -> {
                Dbg.i("server sent resync → full sync")
                performFullSync()
                throw ResyncException()
            }
            else -> Dbg.i("unknown SSE event ${ev.type} ignored")
        }
    }

    private suspend fun touchSync() {
        SyncState.lastSyncAtMs = System.currentTimeMillis()
        updateServiceNotification()
    }

    // ---------------- 事件处理 ----------------

    private suspend fun onMessageEvent(data: String) {
        val msg = runCatching { JSONObject(data) }.getOrElse {
            Dbg.w("bad message json", it); return
        }
        touchSync()
        // notify 缺失 = 维护性变更（已读/未读/全部已读等），仅推进同步进度，
        // 不产生通知也不计入 DND held（bridge.md §3：notify 块是通知判定依据）。
        val notify = msg.optJSONObject("notify") ?: run {
            Dbg.i("message event without notify block: skip")
            return
        }
        val kind = notify.optString("kind").takeIf { it.isNotEmpty() } ?: run {
            Dbg.i("message event without notify.kind: skip")
            return
        }
        val notifyMuted = notify.optBoolean("muted", false)
        val faultId = (notify.optString("faultId").takeIf { it.isNotEmpty() && it != "null" })
            ?: msg.optString("faultId").takeIf { it.isNotEmpty() && it != "null" }
        val msgId = msg.optString("id").ifEmpty { "msg_unknown" }
        val sourceId = msg.optString("sourceId").ifEmpty { "src_unknown" }
        val sourceName = msg.optString("sourceName").ifEmpty { sourceId }
        val title = msg.optString("title").ifEmpty { "(无标题)" }
        val body = msg.optString("body").takeIf { it.isNotEmpty() && it != "null" }
        val incident = msg.optInt("incident", 0)

        // 静音：notify.muted=true 或本地已知静音 → 不通知（lastChangeSeq 已持久化）
        val locallyMuted = faultId?.let { db.notifiedDao().get(it)?.muted == true } == true
        if (notifyMuted || locallyMuted) {
            Dbg.i("muted: skip notify msgId=$msgId faultId=$faultId")
            return
        }

        // DND：窗口内不发声/不振动/不横幅，也不更新已有通知 → 计数 + 保留最后标题
        val cfg = configStore.load()
        if (DndWindow.isActiveAt(
                System.currentTimeMillis(),
                cfg.dndEnabled, cfg.dndStart, cfg.dndEnd, cfg.dndTimezone,
            )
        ) {
            db.heldDao().insert(
                HeldEntity(
                    sourceId = sourceId, sourceName = sourceName,
                    title = title, kind = kind,
                    createdAt = System.currentTimeMillis(),
                ),
            )
            SyncState.heldCount = db.heldDao().count()
            Dbg.i("dnd: held notification kind=$kind title=$title held=${SyncState.heldCount}")
            emitState(cfg.lastChangeSeq)
            return
        }

        when (kind) {
            "new_message" ->
                NotificationHelper.postMessage(this, msgId, sourceId, title, body)

            "incident_open" -> {
                if (faultId == null) {
                    NotificationHelper.postMessage(this, msgId, sourceId, title, body)
                    return
                }
                if (!debounce.allow(faultId)) {
                    Dbg.i("debounced incident_open faultId=$faultId"); return
                }
                NotificationHelper.postFault(this, faultId, title, body, update = false, resolved = false)
                recordNotified(faultId, incident)
            }

            "incident_update" -> {
                if (faultId == null) return
                if (!debounce.allow(faultId)) {
                    Dbg.i("debounced incident_update faultId=$faultId"); return
                }
                NotificationHelper.postFault(this, faultId, title, body, update = true, resolved = false)
                recordNotified(faultId, incident)
            }

            "incident_resolved" -> {
                if (faultId == null) return
                // resolved 终态不可被防抖吞掉：通知内容必须始终反映最新状态（events.md §4）。
                NotificationHelper.postFault(this, faultId, title, body, update = true, resolved = true)
                recordNotified(faultId, incident)
            }

            else -> {
                Dbg.w("unknown notify.kind=$kind: skip")
            }
        }
    }

    /** incident 通知已投递：记录/刷新轮次与最后通知时间，保留既有 muted 状态。 */
    private suspend fun recordNotified(faultId: String, incident: Int) {
        val prev = db.notifiedDao().get(faultId)
        db.notifiedDao().upsert(
            NotifiedEntity(
                faultId = faultId,
                incident = incident,
                lastNotifiedAt = System.currentTimeMillis(),
                muted = prev?.muted ?: false,
            ),
        )
    }

    /** fault 事件：刷新本地静音映射（bridge.md §4 notified 表）。 */
    private suspend fun onFaultEvent(data: String) {
        val fault = runCatching { JSONObject(data) }.getOrElse { return }
        touchSync()
        val id = fault.optString("id")
        if (id.isEmpty()) return
        val muted = fault.optString("mutedAt").let { it.isNotEmpty() && it != "null" }
        val incident = fault.optInt("incident", 0)
        val existing = db.notifiedDao().get(id)
        db.notifiedDao().upsert(
            NotifiedEntity(
                faultId = id,
                incident = incident,
                lastNotifiedAt = existing?.lastNotifiedAt ?: 0,
                muted = muted,
            ),
        )
        Dbg.i("fault event: $id muted=$muted incident=$incident")
    }

    /** settings 事件：携带 dnd 时刷新本地副本（离线也按本地副本执行）。 */
    private suspend fun onSettingsEvent(data: String) {
        val settings = runCatching { JSONObject(data) }.getOrElse { return }
        touchSync()
        val dnd = settings.optJSONObject("dnd") ?: return
        configStore.update { c ->
            c.copy(
                dndEnabled = dnd.optBoolean("enabled", c.dndEnabled),
                dndStart = dnd.optString("start", c.dndStart),
                dndEnd = dnd.optString("end", c.dndEnd),
                dndTimezone = dnd.optString("timezone", c.dndTimezone),
            )
        }
        dndPoke.trySend(Unit)
        Dbg.i("settings event: dnd updated from server")
    }

    // ---------------- DND ----------------

    /** 周期检查 DND 窗口进/出；窗口结束 → held 汇总成一条 assistant.summary。 */
    private fun startTicker() {
        if (tickerJob?.isActive == true) return
        tickerJob = scope.launch {
            // 初始视为「上周期在窗口内」：进程跨越窗口结束重启时触发一次
            // wasActive→!active 迁移，把遗留 held 汇总发出而非滞留一整天。
            var wasActive = true
            while (isActive && !stopping) {
                val cfg = configStore.load()
                val active = DndWindow.isActiveAt(
                    System.currentTimeMillis(),
                    cfg.dndEnabled, cfg.dndStart, cfg.dndEnd, cfg.dndTimezone,
                )
                if (active != wasActive) {
                    Dbg.i("dnd window ${if (active) "entered" else "left"}")
                    if (wasActive && !active) flushHeld(cfg)
                    wasActive = active
                }
                SyncState.dndActive = active
                SyncState.heldCount = db.heldDao().count()
                updateServiceNotification()
                emitState(cfg.lastChangeSeq)
                withTimeoutOrNull(30_000) { dndPoke.receive() }
            }
        }
    }

    private suspend fun flushHeld(cfg: ConfigEntity) {
        val count = db.heldDao().count()
        if (count <= 0) {
            SyncState.heldCount = 0
            return
        }
        val perSource = db.heldDao().perSourceCounts().map { it.sourceName to it.cnt }
        val lastTitle = db.heldDao().latest()?.title
        NotificationHelper.postDndSummary(this, count, perSource, lastTitle)
        db.heldDao().clear()
        SyncState.heldCount = 0
        NativeEvents.emitSummary(count)
        Dbg.i("dnd summary delivered=$count sources=$perSource")
    }

    // ---------------- 全量重同步 ----------------

    /** resync 后走 /api/v1/sync 追平（仅推进 lastChangeSeq + 刷新静音映射）。 */
    private suspend fun performFullSync() {
        val cfg = configStore.load()
        if (cfg.hubUrl.isBlank() || cfg.token.isBlank()) return
        var since = 0L
        var pages = 0
        while (pages < 50) {
            val url = cfg.hubUrl.trimEnd('/') + "/api/v1/sync?since=$since&limit=200"
            val req = Request.Builder().url(url)
                .header("Authorization", "Bearer ${cfg.token}").build()
            val resp = restClient.newCall(req).execute()
            if (resp.code != 200) {
                Dbg.w("sync HTTP ${resp.code}"); resp.close(); return
            }
            val body = resp.body?.string() ?: "{}"
            resp.close()
            val json = JSONObject(body)
            val changes = json.optJSONArray("changes")
            var maxSeq = since
            if (changes != null) {
                for (i in 0 until changes.length()) {
                    val ch = changes.optJSONObject(i) ?: continue
                    val seq = ch.optLong("changeSeq", 0)
                    if (seq > maxSeq) maxSeq = seq
                    if (ch.optString("type") == "fault") {
                        ch.optJSONObject("data")?.let { f ->
                            val fid = f.optString("id")
                            if (fid.isNotEmpty()) {
                                val muted = f.optString("mutedAt").let { it.isNotEmpty() && it != "null" }
                                db.notifiedDao().updateMute(fid, muted, f.optInt("incident", 0))
                            }
                        }
                    }
                }
            }
            configStore.update { c -> c.copy(lastChangeSeq = maxSeq) }
            if (!json.optBoolean("hasMore", false)) break
            since = json.optLong("cursor", maxSeq)
            pages++
        }
        Dbg.i("full sync done lastChangeSeq=${configStore.load().lastChangeSeq}")
    }

    // ---------------- 辅助 ----------------

    private fun serviceSubtitle(): String {
        val reachable = if (SyncState.hubReachable) "已连接" else "未连接"
        val sync = SyncState.lastSyncAtMs?.let { clockFmt.format(Date(it)) } ?: "—"
        return "最近同步 $sync · $reachable"
    }

    private fun updateServiceNotification() {
        NotificationHelper.updateServiceNotification(this, serviceSubtitle())
    }

    private suspend fun emitState(seqOverride: Long? = null) {
        val seq = seqOverride ?: configStore.load().lastChangeSeq
        SyncState.emit(seq)
    }
}
