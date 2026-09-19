package com.sakurasep.assistant

import android.content.Context
import androidx.work.Constraints
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.NetworkType
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import com.sakurasep.assistant.data.NativeDb
import com.sakurasep.assistant.util.Dbg
import java.util.concurrent.TimeUnit

/**
 * 15min 周期兜底（bridge.md §3）：防 SSE 僵死与系统误杀后自愈。
 * 用户曾启动服务且当前未运行 → 拉起；运行中但长时间不可达 → poke 重连。
 */
class SyncWatchdogWorker(
    context: Context,
    params: WorkerParameters,
) : CoroutineWorker(context, params) {

    override suspend fun doWork(): Result {
        val cfg = NativeDb.get(applicationContext).configDao().get()
        if (cfg?.serviceRequested != true) {
            Dbg.i("watchdog: service not requested, skip")
            return Result.success()
        }
        if (!SyncState.running) {
            Dbg.i("watchdog: service requested but not running → restart")
            runCatching { AssistantSyncService.start(applicationContext) }
                .onFailure { Dbg.w("watchdog restart failed (system limit?)", it) }
        } else if (!SyncState.hubReachable) {
            Dbg.i("watchdog: running but unreachable → poke reconnect")
            runCatching { AssistantSyncService.requestReconnect(applicationContext) }
        } else {
            Dbg.i("watchdog: healthy")
        }
        return Result.success()
    }

    companion object {
        private const val WORK_NAME = "assistant_sync_watchdog"

        fun enqueue(context: Context) {
            val req = PeriodicWorkRequestBuilder<SyncWatchdogWorker>(15, TimeUnit.MINUTES)
                .setConstraints(
                    Constraints.Builder()
                        .setRequiredNetworkType(NetworkType.CONNECTED)
                        .build(),
                )
                .build()
            WorkManager.getInstance(context).enqueueUniquePeriodicWork(
                WORK_NAME, ExistingPeriodicWorkPolicy.KEEP, req,
            )
        }

        fun cancel(context: Context) {
            WorkManager.getInstance(context).cancelUniqueWork(WORK_NAME)
        }
    }
}
