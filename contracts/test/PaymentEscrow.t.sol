// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

import {Test} from "forge-std/Test.sol";
import {Vm} from "forge-std/Vm.sol";
import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {PaymentEscrow} from "../src/PaymentEscrow.sol";

contract MockUSDC is ERC20 {
    constructor() ERC20("Mock USDC", "mUSDC") {}

    function decimals() public pure override returns (uint8) {
        return 6;
    }

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }
}

contract FeeToken is MockUSDC {
    function _update(address from, address to, uint256 value) internal override {
        if (from != address(0) && to != address(0)) {
            uint256 fee = value / 100;
            super._update(from, to, value - fee);
            super._update(from, address(0), fee);
        } else {
            super._update(from, to, value);
        }
    }
}

contract RevertingToken is MockUSDC {
    function transferFrom(address, address, uint256) public pure override returns (bool) {
        revert("token failure");
    }
}

contract ReentrantToken is MockUSDC {
    PaymentEscrow public target;
    bytes32 public targetId;
    bool public armed;
    bool public blocked;

    function arm(PaymentEscrow target_, bytes32 id) external {
        target = target_;
        targetId = id;
        armed = true;
    }

    function transferFrom(address from, address to, uint256 amount) public override returns (bool) {
        bool result = super.transferFrom(from, to, amount);
        if (armed) {
            armed = false;
            try target.claimExpiredRefund(targetId) {}
            catch {
                blocked = true;
            }
        }
        return result;
    }
}

contract PayoutReentrantToken is MockUSDC {
    PaymentEscrow public target;
    bytes32 public targetId;
    bool public armed;
    bool public blocked;

    function arm(PaymentEscrow target_, bytes32 targetId_) external {
        target = target_;
        targetId = targetId_;
        armed = true;
    }

    function transfer(address to, uint256 amount) public override returns (bool) {
        if (armed) {
            armed = false;
            try target.refund(targetId) {}
            catch {
                blocked = true;
            }
        }
        return super.transfer(to, amount);
    }
}

contract ToggleFeeToken is MockUSDC {
    bool public feeOn;

    function setFeeOn(bool value) external {
        feeOn = value;
    }

    function _update(address from, address to, uint256 value) internal override {
        if (feeOn && from != address(0) && to != address(0)) {
            uint256 fee = value / 100;
            super._update(from, to, value - fee);
            super._update(from, address(0), fee);
        } else {
            super._update(from, to, value);
        }
    }
}

contract PaymentEscrowTest is Test {
    address internal constant ADMIN = address(0xA11CE);
    address internal constant OPERATOR = address(0xB0B);
    address internal constant PAUSER = address(0xCAFE);
    address internal constant PAYER = address(0xD00D);
    address internal constant PAYEE = address(0xE00E);
    bytes16 internal constant INTENT = bytes16(uint128(1));

    MockUSDC internal token;
    PaymentEscrow internal escrow;

    function setUp() public {
        token = new MockUSDC();
        escrow = new PaymentEscrow(address(token), ADMIN, OPERATOR, PAUSER);
        token.mint(PAYER, 1_000_000e6);
        vm.prank(PAYER);
        token.approve(address(escrow), type(uint256).max);
    }

    function _fund(uint128 amount, uint64 deadline) internal returns (bytes32 id) {
        vm.prank(PAYER);
        id = escrow.createAndFund(INTENT, PAYEE, amount, deadline);
    }

    function testCreateAndReleaseExactlyOnce() public {
        uint64 deadline = uint64(block.timestamp + 1 days);
        bytes32 id = _fund(100e6, deadline);
        assertEq(id, escrow.escrowIdFor(INTENT, PAYER, PAYEE, 100e6, deadline));
        assertEq(token.balanceOf(address(escrow)), 100e6);
        (,, uint128 amount, uint64 storedDeadline, PaymentEscrow.Status status) = escrow.escrows(id);
        assertEq(amount, 100e6);
        assertEq(storedDeadline, deadline);
        assertEq(uint8(status), uint8(PaymentEscrow.Status.FUNDED));

        vm.prank(OPERATOR);
        escrow.release(id);
        assertEq(token.balanceOf(PAYEE), 100e6);
        assertEq(token.balanceOf(address(escrow)), 0);
        vm.expectRevert();
        vm.prank(OPERATOR);
        escrow.release(id);
        vm.expectRevert();
        vm.prank(OPERATOR);
        escrow.refund(id);
    }

    function testEarlyRefundAndTerminalExclusivity() public {
        bytes32 id = _fund(7e6, uint64(block.timestamp + 1 days));
        vm.prank(OPERATOR);
        escrow.refund(id);
        assertEq(token.balanceOf(address(escrow)), 0);
        assertEq(token.balanceOf(PAYER), 1_000_000e6);
        vm.expectRevert();
        vm.prank(OPERATOR);
        escrow.release(id);
    }

    function testExpiryBoundaryAndPermissionlessRefund() public {
        uint64 deadline = uint64(block.timestamp + 100);
        bytes32 id = _fund(5e6, deadline);
        vm.warp(deadline - 1);
        vm.expectRevert();
        vm.prank(address(0x1234));
        escrow.claimExpiredRefund(id);
        vm.warp(deadline);
        vm.expectRevert();
        vm.prank(OPERATOR);
        escrow.release(id);
        vm.prank(address(0x1234));
        escrow.claimExpiredRefund(id);
        assertEq(token.balanceOf(PAYER), 1_000_000e6);
        vm.expectRevert();
        escrow.claimExpiredRefund(id);
    }

    function testPauseBlocksCreationAndReleaseButNotRefund() public {
        bytes32 id = _fund(3e6, uint64(block.timestamp + 1 days));
        vm.prank(PAUSER);
        escrow.pause();
        vm.expectRevert();
        vm.prank(PAYER);
        escrow.createAndFund(bytes16(uint128(2)), PAYEE, 1e6, uint64(block.timestamp + 1 days));
        vm.expectRevert();
        vm.prank(OPERATOR);
        escrow.release(id);
        vm.prank(OPERATOR);
        escrow.refund(id);
        assertEq(token.balanceOf(address(escrow)), 0);
        vm.expectRevert();
        vm.prank(PAUSER);
        escrow.unpause();
        vm.prank(ADMIN);
        escrow.unpause();
    }

    function testUnauthorizedCallsRevert() public {
        bytes32 id = _fund(2e6, uint64(block.timestamp + 1 days));
        vm.expectRevert();
        vm.prank(PAYER);
        escrow.release(id);
        vm.expectRevert();
        vm.prank(PAYEE);
        escrow.refund(id);
        vm.expectRevert();
        vm.prank(PAYER);
        escrow.pause();
    }

    function testDuplicateAndInvalidInputsRevert() public {
        uint64 deadline = uint64(block.timestamp + 1 days);
        _fund(1e6, deadline);
        vm.expectRevert();
        vm.prank(PAYER);
        escrow.createAndFund(INTENT, PAYEE, 1e6, deadline);
        vm.expectRevert(PaymentEscrow.InvalidAddress.selector);
        vm.prank(PAYER);
        escrow.createAndFund(bytes16(uint128(2)), address(0), 1e6, deadline);
        vm.expectRevert(PaymentEscrow.InvalidAmount.selector);
        vm.prank(PAYER);
        escrow.createAndFund(bytes16(uint128(3)), PAYEE, 0, deadline);
        vm.expectRevert(PaymentEscrow.InvalidExpiry.selector);
        vm.prank(PAYER);
        escrow.createAndFund(bytes16(uint128(4)), PAYEE, 1e6, uint64(block.timestamp));
        vm.expectRevert(PaymentEscrow.InvalidExpiry.selector);
        vm.prank(PAYER);
        escrow.createAndFund(bytes16(uint128(5)), PAYEE, 1e6, uint64(block.timestamp + 30 days + 1));
    }

    function testFeeTokenAndRevertingTokenLeaveNoEscrow() public {
        FeeToken fee = new FeeToken();
        PaymentEscrow feeEscrow = new PaymentEscrow(address(fee), ADMIN, OPERATOR, PAUSER);
        fee.mint(PAYER, 100e6);
        vm.prank(PAYER);
        fee.approve(address(feeEscrow), 100e6);
        bytes32 feeId = feeEscrow.escrowIdFor(INTENT, PAYER, PAYEE, 100e6, uint64(block.timestamp + 1 days));
        vm.expectRevert(abi.encodeWithSelector(PaymentEscrow.UnexpectedTokenBalance.selector, 100e6, 99e6));
        vm.prank(PAYER);
        feeEscrow.createAndFund(INTENT, PAYEE, 100e6, uint64(block.timestamp + 1 days));
        (,,,, PaymentEscrow.Status feeStatus) = feeEscrow.escrows(feeId);
        assertEq(uint8(feeStatus), uint8(PaymentEscrow.Status.NONE));

        RevertingToken reverting = new RevertingToken();
        PaymentEscrow revertingEscrow = new PaymentEscrow(address(reverting), ADMIN, OPERATOR, PAUSER);
        reverting.mint(PAYER, 100e6);
        vm.prank(PAYER);
        reverting.approve(address(revertingEscrow), 100e6);
        vm.expectRevert();
        vm.prank(PAYER);
        revertingEscrow.createAndFund(INTENT, PAYEE, 100e6, uint64(block.timestamp + 1 days));
        bytes32 revertingId = revertingEscrow.escrowIdFor(INTENT, PAYER, PAYEE, 100e6, uint64(block.timestamp + 1 days));
        (,,,, PaymentEscrow.Status revertingStatus) = revertingEscrow.escrows(revertingId);
        assertEq(uint8(revertingStatus), uint8(PaymentEscrow.Status.NONE));
    }

    function testReentrantTokenCannotRefundDuringFunding() public {
        ReentrantToken reentrant = new ReentrantToken();
        PaymentEscrow guarded = new PaymentEscrow(address(reentrant), ADMIN, OPERATOR, PAUSER);
        reentrant.mint(PAYER, 10e6);
        vm.prank(PAYER);
        reentrant.approve(address(guarded), 10e6);

        vm.prank(PAYER);
        bytes32 first = guarded.createAndFund(INTENT, PAYEE, 2e6, uint64(block.timestamp + 2));
        vm.warp(block.timestamp + 2);
        reentrant.arm(guarded, first);
        vm.prank(PAYER);
        guarded.createAndFund(bytes16(uint128(2)), PAYEE, 3e6, uint64(block.timestamp + 1 days));
        assertTrue(reentrant.blocked());
        (,,,, PaymentEscrow.Status status) = guarded.escrows(first);
        assertEq(uint8(status), uint8(PaymentEscrow.Status.FUNDED));
        assertEq(reentrant.balanceOf(address(guarded)), 5e6);
    }

    function testPayoutCannotReenterAnotherFundedEscrow() public {
        PayoutReentrantToken reentrant = new PayoutReentrantToken();
        PaymentEscrow guarded = new PaymentEscrow(address(reentrant), ADMIN, address(reentrant), PAUSER);
        reentrant.mint(PAYER, 10e6);
        vm.prank(PAYER);
        reentrant.approve(address(guarded), 10e6);
        uint64 deadline = uint64(block.timestamp + 1 days);
        vm.prank(PAYER);
        bytes32 first = guarded.createAndFund(INTENT, PAYEE, 2e6, deadline);
        vm.prank(PAYER);
        bytes32 second = guarded.createAndFund(bytes16(uint128(2)), PAYEE, 3e6, deadline);
        reentrant.arm(guarded, second);

        vm.prank(address(reentrant));
        guarded.release(first);
        assertTrue(reentrant.blocked());
        (,,,, PaymentEscrow.Status firstStatus) = guarded.escrows(first);
        (,,,, PaymentEscrow.Status secondStatus) = guarded.escrows(second);
        assertEq(uint8(firstStatus), uint8(PaymentEscrow.Status.RELEASED));
        assertEq(uint8(secondStatus), uint8(PaymentEscrow.Status.FUNDED));
        assertEq(reentrant.balanceOf(PAYEE), 2e6);
        assertEq(reentrant.balanceOf(address(guarded)), 3e6);
    }

    function testInterleavedTerminalCallsPreserveTwoEscrows() public {
        uint64 deadline = uint64(block.timestamp + 1 days);
        bytes32 first = _fund(2e6, deadline);
        vm.prank(PAYER);
        bytes32 second = escrow.createAndFund(bytes16(uint128(2)), PAYEE, 3e6, deadline);
        vm.prank(OPERATOR);
        escrow.release(first);
        vm.expectRevert();
        vm.prank(OPERATOR);
        escrow.refund(first);
        vm.prank(OPERATOR);
        escrow.refund(second);
        vm.expectRevert();
        vm.prank(OPERATOR);
        escrow.release(second);
        assertEq(token.balanceOf(PAYEE), 2e6);
        assertEq(token.balanceOf(PAYER), 1_000_000e6 - 2e6);
        assertEq(token.balanceOf(address(escrow)), 0);
    }

    function testEventSequenceCanReconstructExpiryRefund() public {
        uint64 deadline = uint64(block.timestamp + 10);
        bytes32 id = escrow.escrowIdFor(INTENT, PAYER, PAYEE, 4e6, deadline);
        vm.recordLogs();
        _fund(4e6, deadline);
        Vm.Log[] memory creationLogs = vm.getRecordedLogs();
        uint256 created;
        uint256 funded;
        for (uint256 i; i < creationLogs.length; ++i) {
            if (creationLogs[i].emitter != address(escrow)) continue;
            if (creationLogs[i].topics[0] == keccak256("EscrowCreated(bytes32,address,address,address,uint256,uint64)"))
            {
                assertEq(creationLogs[i].topics[1], id);
                assertEq(creationLogs[i].topics[2], bytes32(uint256(uint160(PAYER))));
                assertEq(creationLogs[i].topics[3], bytes32(uint256(uint160(PAYEE))));
                (address loggedToken, uint256 loggedAmount, uint64 loggedDeadline) =
                    abi.decode(creationLogs[i].data, (address, uint256, uint64));
                assertEq(loggedToken, address(token));
                assertEq(loggedAmount, 4e6);
                assertEq(loggedDeadline, deadline);
                ++created;
            }
            if (creationLogs[i].topics[0] == keccak256("EscrowFunded(bytes32,uint256)")) {
                assertEq(creationLogs[i].topics[1], id);
                assertEq(abi.decode(creationLogs[i].data, (uint256)), 4e6);
                ++funded;
            }
        }
        assertEq(created, 1);
        assertEq(funded, 1);

        vm.warp(deadline);
        vm.recordLogs();
        escrow.claimExpiredRefund(id);
        Vm.Log[] memory refundLogs = vm.getRecordedLogs();
        uint256 expired;
        uint256 refunded;
        for (uint256 i; i < refundLogs.length; ++i) {
            if (refundLogs[i].emitter != address(escrow)) continue;
            if (refundLogs[i].topics[0] == keccak256("EscrowExpired(bytes32)")) {
                assertEq(refundLogs[i].topics[1], id);
                ++expired;
            }
            if (refundLogs[i].topics[0] == keccak256("EscrowRefunded(bytes32,address,uint256)")) {
                assertEq(refundLogs[i].topics[1], id);
                assertEq(refundLogs[i].topics[2], bytes32(uint256(uint160(PAYER))));
                assertEq(abi.decode(refundLogs[i].data, (uint256)), 4e6);
                ++refunded;
            }
        }
        assertEq(expired, 1);
        assertEq(refunded, 1);
    }

    function testReleaseEventIdentifiesRecipientAndAmount() public {
        bytes32 id = _fund(6e6, uint64(block.timestamp + 1 days));
        vm.recordLogs();
        vm.prank(OPERATOR);
        escrow.release(id);
        Vm.Log[] memory logs = vm.getRecordedLogs();
        uint256 released;
        for (uint256 i; i < logs.length; ++i) {
            if (logs[i].emitter != address(escrow)) continue;
            if (logs[i].topics[0] != keccak256("EscrowReleased(bytes32,address,uint256)")) continue;
            assertEq(logs[i].topics[1], id);
            assertEq(logs[i].topics[2], bytes32(uint256(uint160(PAYEE))));
            assertEq(abi.decode(logs[i].data, (uint256)), 6e6);
            ++released;
        }
        assertEq(released, 1);
    }

    function testDelayedTwoStepAdminTransfer() public {
        address nextAdmin = address(0xA2);
        assertEq(escrow.defaultAdminDelay(), 2 days);
        vm.prank(ADMIN);
        escrow.beginDefaultAdminTransfer(nextAdmin);
        (address pending, uint48 schedule) = escrow.pendingDefaultAdmin();
        assertEq(pending, nextAdmin);
        vm.expectRevert();
        vm.prank(nextAdmin);
        escrow.acceptDefaultAdminTransfer();
        vm.warp(uint256(schedule) + 1);
        vm.prank(nextAdmin);
        escrow.acceptDefaultAdminTransfer();
        assertEq(escrow.defaultAdmin(), nextAdmin);
        vm.prank(PAUSER);
        escrow.pause();
        vm.expectRevert();
        vm.prank(ADMIN);
        escrow.unpause();
        vm.prank(nextAdmin);
        escrow.unpause();
    }

    function testExpiredRefundWorksWhilePausedAndOperatorMayRefundAfterExpiry() public {
        uint64 deadline = uint64(block.timestamp + 10);
        bytes32 first = _fund(2e6, deadline);
        vm.prank(PAYER);
        bytes32 second = escrow.createAndFund(bytes16(uint128(2)), PAYEE, 3e6, deadline);
        vm.prank(PAUSER);
        escrow.pause();
        vm.warp(deadline);
        escrow.claimExpiredRefund(first);
        vm.prank(OPERATOR);
        escrow.refund(second);
        assertEq(token.balanceOf(address(escrow)), 0);
        assertEq(token.balanceOf(PAYER), 1_000_000e6);
    }

    function testTokenSwitchingToFeeModeCannotShortPayRecipient() public {
        ToggleFeeToken changing = new ToggleFeeToken();
        PaymentEscrow guarded = new PaymentEscrow(address(changing), ADMIN, OPERATOR, PAUSER);
        changing.mint(PAYER, 10e6);
        vm.prank(PAYER);
        changing.approve(address(guarded), 10e6);
        vm.prank(PAYER);
        bytes32 id = guarded.createAndFund(INTENT, PAYEE, 5e6, uint64(block.timestamp + 1 days));
        changing.setFeeOn(true);
        vm.expectRevert();
        vm.prank(OPERATOR);
        guarded.release(id);
        (,,,, PaymentEscrow.Status status) = guarded.escrows(id);
        assertEq(uint8(status), uint8(PaymentEscrow.Status.FUNDED));
        assertEq(changing.balanceOf(address(guarded)), 5e6);
        assertEq(changing.balanceOf(PAYEE), 0);
    }

    function testFuzz_DuplicateIntent(bytes16 intent) public {
        uint64 deadline = uint64(block.timestamp + 1 days);
        vm.prank(PAYER);
        bytes32 id = escrow.createAndFund(intent, PAYEE, 1e6, deadline);
        vm.expectRevert(abi.encodeWithSelector(PaymentEscrow.EscrowAlreadyExists.selector, id));
        vm.prank(PAYER);
        escrow.createAndFund(intent, PAYEE, 1e6, deadline);
    }

    function testSameUUIDWithDifferentPayerOrTermsCannotOccupyExpectedID() public {
        uint64 deadline = uint64(block.timestamp + 1 days);
        address stranger = address(0xBAD);
        token.mint(stranger, 10e6);
        vm.prank(stranger);
        token.approve(address(escrow), 10e6);

        bytes32 expected = escrow.escrowIdFor(INTENT, PAYER, PAYEE, 2e6, deadline);
        vm.prank(stranger);
        bytes32 strangerId = escrow.createAndFund(INTENT, PAYEE, 2e6, deadline);
        assertTrue(strangerId != expected);

        vm.prank(PAYER);
        bytes32 alteredTerms = escrow.createAndFund(INTENT, PAYEE, 1e6, deadline);
        assertTrue(alteredTerms != expected);

        vm.prank(PAYER);
        bytes32 fundedExpected = escrow.createAndFund(INTENT, PAYEE, 2e6, deadline);
        assertEq(fundedExpected, expected);
        assertEq(token.balanceOf(address(escrow)), 5e6);
    }

    function testFuzz_UnauthorizedCaller(address caller) public {
        vm.assume(caller != OPERATOR);
        bytes32 id = _fund(1e6, uint64(block.timestamp + 1 days));
        vm.expectRevert();
        vm.prank(caller);
        escrow.release(id);
        vm.expectRevert();
        vm.prank(caller);
        escrow.refund(id);
        assertEq(token.balanceOf(address(escrow)), 1e6);
    }

    function testFuzz_AmountAndDeadline(uint128 amount, uint32 delta) public {
        amount = uint128(bound(amount, 1, 1_000_000e6));
        delta = uint32(bound(delta, 1, 30 days));
        bytes32 id = _fund(amount, uint64(block.timestamp + delta));
        assertEq(token.balanceOf(address(escrow)), amount);
        vm.prank(OPERATOR);
        escrow.refund(id);
        assertEq(token.balanceOf(address(escrow)), 0);
        assertEq(token.balanceOf(PAYER), 1_000_000e6);
    }
}
