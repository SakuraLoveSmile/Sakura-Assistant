package com.sakurasep.assistant

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.os.PowerManager
import com.sakurasep.assistant.data.NativeDb
import com.sakurasep.assistant.util.Dbg
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch

/**
 * BOOT_COMPLETED / MY_PACKAGE_REPLACED（bridge.md §3/§5）：
 * 仅当用户曾启动服务（serviceRequested）才拉起前台服务；
 * 「强行停止」后系统不再投递广播，不承诺拉起（设置页明示）。
 */
class BootReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        val action = intent.action ?: return
        if (action != Intent.ACTION_BOOT_COMPLETED &&
            action != Intent.ACTION_MY_PACKAGE_REPLACED
        ) return

        Dbg.i("BootReceiver onReceive: $action")
        val pending = goAsync()
        // 拉起前台服务前短暂持锁（WAKE_LOCK 仅必要短时，bridge.md §5）
        val wakeLock = runCatching {
            val pm = context.getSystemService(Context.POWER_SERVICE) as PowerManager
            @Suppress("WakelockTimeout")
            pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "assistant:boot").apply {
                acquire(15_000L)
            }
        }.getOrNull()

        CoroutineScope(Dispatchers.IO).launch {
            try {
                val requested = runCatching {
                    NativeDb.get(context).configDao().get()?.serviceRequested == true
                }.getOrDefault(false)
                if (requested) {
                    Dbg.i("boot: serviceRequested=true → startForegroundService")
                    runCatching { AssistantSyncService.start(context) }
                        .onFailure {
                            // 如被 HyperOS 自启动限制拦截，属系统限制，如实记录
                            Dbg.w("boot start service blocked: ${it.javaClass.simpleName} ${it.message}")
                        }
                } else {
                    Dbg.i("boot: serviceRequested=false, skip")
                }
            } finally {
                runCatching { wakeLock?.let { if (it.isHeld) it.release() } }
                pending.finish()
            }
        }
    }
}
