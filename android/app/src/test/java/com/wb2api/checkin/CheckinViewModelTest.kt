package com.wb2api.checkin

import com.wb2api.checkin.data.Account
import com.wb2api.checkin.data.CheckinResult
import com.wb2api.checkin.data.EngineClient
import com.wb2api.checkin.data.ImportFile
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class CheckinViewModelTest {
    private val dispatcher = StandardTestDispatcher()

    @get:Rule
    val main = MainDispatcherRule(dispatcher)

    private class FakeEngine(
        var accounts: List<Account> = listOf(
            Account(uid = "u1", nickname = "一号"),
            Account(uid = "u2", nickname = "二号"),
        ),
    ) : EngineClient {
        val checked = mutableListOf<String>()
        var throwFor: String? = null

        override fun listAccounts(): List<Account> = accounts

        override fun importAccount(content: String): Account {
            val acc = Account(uid = content.trim(), nickname = content.trim())
            accounts = accounts + acc
            return acc
        }

        override fun checkin(uid: String): CheckinResult {
            checked += uid
            if (uid == throwFor) error("boom")
            return CheckinResult(uid = uid, status = "ok", message = "签到成功")
        }
    }

    private fun viewModel(engine: FakeEngine) =
        CheckinViewModel(engine, io = UnconfinedTestDispatcher())

    @Test
    fun `loads accounts on init`() {
        val vm = viewModel(FakeEngine())
        assertEquals(listOf("u1", "u2"), vm.state.value.rows.map { it.account.uid })
        assertTrue(!vm.state.value.loading)
    }

    @Test
    fun `checkin all visits every account in order`() {
        val engine = FakeEngine()
        val vm = viewModel(engine)

        vm.checkinAll()

        assertEquals(listOf("u1", "u2"), engine.checked)
        assertEquals(2, vm.state.value.progressDone)
        assertEquals(2, vm.state.value.progressTotal)
        assertTrue(vm.state.value.rows.all { it.result?.status == "ok" })
        assertTrue(!vm.state.value.busy)
    }

    @Test
    fun `checkin all continues after an exception`() {
        val engine = FakeEngine().apply { throwFor = "u1" }
        val vm = viewModel(engine)

        vm.checkinAll()

        assertEquals(listOf("u1", "u2"), engine.checked)
        assertEquals("failed", vm.state.value.rows[0].result?.status)
        assertEquals("ok", vm.state.value.rows[1].result?.status)
    }

    @Test
    fun `rapid checkin all calls do not overlap`() {
        val engine = FakeEngine()
        val vm = CheckinViewModel(engine, io = dispatcher)
        dispatcher.scheduler.advanceUntilIdle()

        vm.checkinAll()
        vm.checkinAll()
        dispatcher.scheduler.advanceUntilIdle()

        assertEquals(listOf("u1", "u2"), engine.checked)
        assertEquals(2, vm.state.value.progressTotal)
        assertTrue(!vm.state.value.busy)
    }

    @Test
    fun `import reports read failures and refreshes rows`() {
        val engine = FakeEngine()
        val vm = viewModel(engine)

        vm.importFiles(
            listOf(
                ImportFile("a.json", "u3"),
                ImportFile("b.json", null),
            ),
        )

        val summary = vm.state.value.importSummary
        assertNotNull(summary)
        assertEquals(2, summary!!.total)
        assertEquals(1, summary.ok)
        assertEquals(1, summary.failures.size)
        assertEquals(listOf("u1", "u2", "u3"), vm.state.value.rows.map { it.account.uid })
    }
}
