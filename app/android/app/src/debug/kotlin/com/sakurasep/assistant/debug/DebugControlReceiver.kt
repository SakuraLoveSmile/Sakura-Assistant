package com.sakurasep.assistant.debug

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import com.sakurasep.assistant.AssistantSyncService
import com.sakurasep.assistant.data.ConfigStore
import com.sakurasep.assistant.util.Dbg
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch

/**
 * 仅 debug 构建：经 adb 广播注入配置 / 启停服务，绕开未就绪的 Dart 侧做实机验证。
 *
 * 用法：
 *   adb shell am broadcast -a com.sakurasep.assistant.debug.CONFIGURE \
 *     --es hubUrl http://localhost:8795 --es token tok_dev \
 *     --ei reportIntervalSeconds 30 \
 *     --ez dndEnabled false --es dndStart 23:00 --es dndEnd 08:00
 *   adb shell am broadcast -a com.sakurasep.assistant.debug.START_SERVICE
 *   adb shell am broadcast -a com.sakurasep.assistant.debug.STOP_SERVICE
 */
class DebugControlReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        when (intent.action) {
            ACTION_CONFIGURE -> {
                val pending = goAsync()
                CoroutineScope(Dispatchers.IO).launch {
                    try {
                        ConfigStore(context).update { c ->
                            c.copy(
                                hubUrl = intent.getStringExtra("hubUrl") ?: c.hubUrl,
                                token = intent.getStringExtra("token") ?: c.token,
                                reportIntervalSeconds = intent.getIntExtra(
                                    "reportIntervalSeconds", c.reportIntervalSeconds,
                                ),
                                dndEnabled = intent.getBooleanExtra("dndEnabled", c.dndEnabled),
                                dndStart = intent.getStringExtra("dndStart") ?: c.dndStart,
                                dndEnd = intent.getStringExtra("dndEnd") ?: c.dndEnd,
                                dndTimezone = intent.getStringExtra("dndTimezone") ?: c.dndTimezone,
                                debugLogging = intent.getBooleanExtra("debugLogging", c.debugLogging),
                            )
                        }
                        Dbg.enabled = intent.getBooleanExtra("debugLogging", Dbg.enabled)
                        Dbg.i("debug configure applied")
                    } finally {
                        pending.finish()
                    }
                }
            }
            ACTION_START -> {
                val pending = goAsync()
                CoroutineScope(Dispatchers.IO).launch {
                    try {
                        ConfigStore(context).update { it.copy(serviceRequested = true) }
                        runCatching { AssistantSyncService.start(context) }
                        Dbg.i("debug start service")
                    } finally {
                        pending.finish()
                    }
                }
            }
            ACTION_STOP -> {
                val pending = goAsync()
                CoroutineScope(Dispatchers.IO).launch {
                    try {
                        ConfigStore(context).update { it.copy(serviceRequested = false) }
                        runCatching { AssistantSyncService.requestStop(context) }
                        Dbg.i("debug stop service")
                    } finally {
                        pending.finish()
                    }
                }
            }
        }
    }

    companion object {
        const val ACTION_CONFIGURE = "com.sakurasep.assistant.debug.CONFIGURE"
        const val ACTION_START = "com.sakurasep.assistant.debug.START_SERVICE"
        const val ACTION_STOP = "com.sakurasep.assistant.debug.STOP_SERVICE"
    }
}
