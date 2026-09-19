package com.sakurasep.assistant.notify

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import com.sakurasep.assistant.MainActivity
import com.sakurasep.assistant.R
import com.sakurasep.assistant.util.Dbg

/**
 * 通知渠道与发布（bridge.md §3）。渠道一经创建不改 importance（系统限制）。
 */
object NotificationHelper {

    const val CH_MESSAGES = "assistant.messages"
    const val CH_FAULTS = "assistant.faults"
    const val CH_SUMMARY = "assistant.summary"
    const val CH_SERVICE = "assistant.service"

    const val ID_SERVICE = 1

    const val GROUP_FAULTS = "flt"
    const val EXTRA_ROUTE = "route"
    const val EXTRA_ID = "id"

    fun ensureChannels(context: Context) {
        val nm = context.getSystemService(NotificationManager::class.java)
        fun ch(id: String, name: String, importance: Int) =
            NotificationChannel(id, name, importance)
        nm.createNotificationChannels(
            listOf(
                ch(CH_MESSAGES, "消息", NotificationManager.IMPORTANCE_DEFAULT),
                ch(CH_FAULTS, "故障", NotificationManager.IMPORTANCE_HIGH),
                ch(CH_SUMMARY, "免打扰汇总", NotificationManager.IMPORTANCE_DEFAULT),
                ch(CH_SERVICE, "服务状态", NotificationManager.IMPORTANCE_MIN),
            ),
        )
    }

    fun groupForSource(sourceId: String) = "src_$sourceId"

    /** 故障通知 id：契约约定用 faultId hash。 */
    fun faultNotifId(faultId: String) = faultId.hashCode()

    /** 独立消息通知 id：与故障 id 空间错开（消息 hash 高位异或）。 */
    fun messageNotifId(messageId: String) = messageId.hashCode() xor 0x4D534700

    private fun tapIntent(context: Context, route: String, id: String?, requestCode: Int): PendingIntent {
        val intent = Intent(context, MainActivity::class.java).apply {
            putExtra(EXTRA_ROUTE, route)
            if (id != null) putExtra(EXTRA_ID, id)
            addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP)
        }
        return PendingIntent.getActivity(
            context, requestCode, intent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
    }

    /** 常驻服务通知（id=1，渠道 assistant.service / MIN）。 */
    fun buildServiceNotification(context: Context, contentText: String): Notification {
        ensureChannels(context)
        return NotificationCompat.Builder(context, CH_SERVICE)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle("Assistant 服务运行中")
            .setContentText(contentText)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setCategory(NotificationCompat.CATEGORY_SERVICE)
            .setContentIntent(tapIntent(context, "home", null, ID_SERVICE))
            .build()
    }

    fun updateServiceNotification(context: Context, contentText: String) {
        runCatching {
            NotificationManagerCompat.from(context)
                .notify(ID_SERVICE, buildServiceNotification(context, contentText))
        }.onFailure { Dbg.w("updateServiceNotification failed", it) }
    }

    /** 独立消息通知（new_message）：新通知，group src_<sourceId>。 */
    fun postMessage(context: Context, messageId: String, sourceId: String, title: String, body: String?) {
        ensureChannels(context)
        val notif = NotificationCompat.Builder(context, CH_MESSAGES)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(title)
            .setContentText(body)
            .setStyle(NotificationCompat.BigTextStyle().bigText(body ?: title))
            .setAutoCancel(true)
            .setGroup(groupForSource(sourceId))
            .setCategory(NotificationCompat.CATEGORY_MESSAGE)
            .setContentIntent(tapIntent(context, "message", messageId, messageNotifId(messageId)))
            .build()
        NotificationManagerCompat.from(context).notify(messageNotifId(messageId), notif)
        Dbg.timed("posted message notification id=$messageId title=$title")
    }

    /**
     * 故障通知：kind=open 新通知；update/resolved 用 onlyAlertOnce 更新同 id。
     */
    fun postFault(
        context: Context,
        faultId: String,
        title: String,
        body: String?,
        update: Boolean,
        resolved: Boolean,
    ) {
        ensureChannels(context)
        val displayTitle = if (resolved) "$title（已恢复）" else title
        val notif = NotificationCompat.Builder(context, CH_FAULTS)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(displayTitle)
            .setContentText(body)
            .setStyle(NotificationCompat.BigTextStyle().bigText(body ?: displayTitle))
            .setAutoCancel(true)
            .setGroup(GROUP_FAULTS)
            .setOnlyAlertOnce(update || resolved)
            .setCategory(NotificationCompat.CATEGORY_ALARM)
            .setContentIntent(tapIntent(context, "fault", faultId, faultNotifId(faultId)))
            .build()
        NotificationManagerCompat.from(context).notify(faultNotifId(faultId), notif)
        Dbg.timed("posted fault notification faultId=$faultId resolved=$resolved title=$displayTitle")
    }

    /** DND 结束汇总（assistant.summary 一条；route=home）。 */
    fun postDndSummary(context: Context, heldCount: Int, perSource: List<Pair<String, Int>>, lastTitle: String?) {
        ensureChannels(context)
        val breakdown = perSource.joinToString(" · ") { "${it.first} ×${it.second}" }
        val text = buildString {
            append("免打扰期间共 $heldCount 条通知")
            if (breakdown.isNotEmpty()) append("：$breakdown")
            if (!lastTitle.isNullOrEmpty()) append("。最近：$lastTitle")
        }
        val notif = NotificationCompat.Builder(context, CH_SUMMARY)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle("Assistant 免打扰汇总")
            .setContentText(text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setAutoCancel(true)
            .setCategory(NotificationCompat.CATEGORY_STATUS)
            .setContentIntent(tapIntent(context, "home", null, ID_DND_SUMMARY))
            .build()
        NotificationManagerCompat.from(context).notify(ID_DND_SUMMARY, notif)
        Dbg.i("posted DND summary count=$heldCount")
    }

    fun cancelServiceNotification(context: Context) {
        NotificationManagerCompat.from(context).cancel(ID_SERVICE)
    }

    private const val ID_DND_SUMMARY = 2
}
